package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
)

type message struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data string `json:"data"`
}

type socketAction struct {
	Action string  `json:"action"` // "publish" or "subscribe"
	Queue  string  `json:"queue"`
	Msg    message `json:"message,omitempty"`
}

const (
	persistenceFile = "queues.json"
	tcpPort         = ":8003"
)

var (
	// queues stores messages keyed by queue name
	queues = make(map[string][]message)
	// subscribers stores active channels for fan-out delivery
	subscribers = make(map[string][]chan message)
	mu          sync.Mutex
)

func init() {
	loadQueues()
}

func loadQueues() {
	if _, err := os.Stat(persistenceFile); os.IsNotExist(err) {
		return
	}

	data, err := ioutil.ReadFile(persistenceFile)
	if err != nil {
		log.Printf("Error reading persistence file: %v", err)
		return
	}

	mu.Lock()
	defer mu.Unlock()
	if err := json.Unmarshal(data, &queues); err != nil {
		log.Printf("Error unmarshaling persistence data: %v", err)
		return
	}
	log.Printf("Loaded %d queues from persistence", len(queues))
}

func saveQueues() {
	mu.Lock()
	data, err := json.MarshalIndent(queues, "", "  ")
	mu.Unlock()

	if err != nil {
		log.Printf("Error marshaling queues for persistence: %v", err)
		return
	}

	if err := ioutil.WriteFile(persistenceFile, data, 0644); err != nil {
		log.Printf("Error writing persistence file: %v", err)
	}
}

func getQueueName(path string) string {
	name := strings.TrimPrefix(path, "/")
	if name == "" {
		return "default"
	}
	return name
}

func publishersHandler(rw http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		http.Error(rw, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	queueName := getQueueName(req.URL.Path)

	var m message
	decoder := json.NewDecoder(req.Body)
	if err := decoder.Decode(&m); err != nil {
		http.Error(rw, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	handlePublish(queueName, m)

	rw.WriteHeader(http.StatusCreated)

	mJSON, _ := json.Marshal(m)
	log.Printf("[HTTP][%s] New message '%s'", queueName, mJSON)
}

func handlePublish(queueName string, m message) {
	mu.Lock()
	// Fan-out: Send to all active subscribers
	subs := subscribers[queueName]
	if len(subs) > 0 {
		for _, ch := range subs {
			ch <- m
		}
		// Clear subscribers after delivery (one-shot delivery per poll)
		subscribers[queueName] = nil
	} else {
		// If no active subscribers, queue the message
		queues[queueName] = append(queues[queueName], m)
	}
	mu.Unlock()

	saveQueues()
}

func subscribersHandler(rw http.ResponseWriter, req *http.Request) {
	if req.Method != "GET" {
		http.Error(rw, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	queueName := getQueueName(req.URL.Path)

	mu.Lock()
	q, ok := queues[queueName]
	if ok && len(q) > 0 {
		// Pull from queue if messages exist
		m := q[0]
		queues[queueName] = q[1:]
		currentLen := len(queues[queueName])
		mu.Unlock()

		saveQueues()
		deliverMessage(rw, m, queueName, currentLen)
		return
	}

	// No messages in queue, wait for a new one (Long Polling / Fan-out)
	ch := make(chan message, 1)
	subscribers[queueName] = append(subscribers[queueName], ch)
	mu.Unlock()

	select {
	case m := <-ch:
		deliverMessage(rw, m, queueName, 0)
	case <-req.Context().Done():
		// Client disconnected, cleanup channel
		mu.Lock()
		subs := subscribers[queueName]
		for i, s := range subs {
			if s == ch {
				subscribers[queueName] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
		mu.Unlock()
	}
}

func deliverMessage(rw http.ResponseWriter, m message, queueName string, currentLen int) {
	mJSON, err := json.Marshal(m)
	if err != nil {
		http.Error(rw, "Internal server error", http.StatusInternalServerError)
		return
	}

	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	rw.Write(mJSON)

	log.Printf("[HTTP][%s] Message delivered. Remaining in queue: %d", queueName, currentLen)
}

func startSocketServer() {
	ln, err := net.Listen("tcp", tcpPort)
	if err != nil {
		log.Fatalf("Socket server failed: %v", err)
	}
	log.Printf("Socket server starting on %s", tcpPort)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("Socket accept error: %v", err)
			continue
		}
		go handleSocketConn(conn)
	}
}

func handleSocketConn(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		var action socketAction
		if err := json.Unmarshal(scanner.Bytes(), &action); err != nil {
			fmt.Fprintf(conn, `{"error": "Invalid JSON"}`+"\n")
			continue
		}

		switch action.Action {
		case "publish":
			handlePublish(action.Queue, action.Msg)
			fmt.Fprintf(conn, `{"status": "published"}`+"\n")

		case "subscribe":
			handleSocketSubscribe(conn, action.Queue)
			return // handleSocketSubscribe takes over the connection

		default:
			fmt.Fprintf(conn, `{"error": "Unknown action"}`+"\n")
		}
	}
}

func handleSocketSubscribe(conn net.Conn, queueName string) {
	mu.Lock()
	q, ok := queues[queueName]
	if ok && len(q) > 0 {
		m := q[0]
		queues[queueName] = q[1:]
		mu.Unlock()
		saveQueues()
		mJSON, _ := json.Marshal(m)
		fmt.Fprintf(conn, string(mJSON)+"\n")
		return
	}

	ch := make(chan message, 1)
	subscribers[queueName] = append(subscribers[queueName], ch)
	mu.Unlock()

	m := <-ch
	mJSON, _ := json.Marshal(m)
	fmt.Fprintf(conn, string(mJSON)+"\n")
}

func main() {
	publishersServer := http.NewServeMux()
	publishersServer.HandleFunc("/", publishersHandler)

	subscribersServer := http.NewServeMux()
	subscribersServer.HandleFunc("/", subscribersHandler)

	wg := sync.WaitGroup{}
	wg.Add(3)

	go func() {
		defer wg.Done()
		log.Println("Publisher server starting on :8001")
		if err := http.ListenAndServe(":8001", publishersServer); err != nil {
			log.Fatalf("Publisher server failed: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		log.Println("Subscriber server starting on :8002")
		if err := http.ListenAndServe(":8002", subscribersServer); err != nil {
			log.Fatalf("Subscriber server failed: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		startSocketServer()
	}()

	wg.Wait()
}
