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

func (m message) Validate() error {
	if m.ID == "" {
		return fmt.Errorf("message id is required")
	}
	if m.Type == "" {
		return fmt.Errorf("message type is required")
	}
	return nil
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
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	loadQueues()
}

func loadQueues() {
	if _, err := os.Stat(persistenceFile); os.IsNotExist(err) {
		return
	}

	data, err := ioutil.ReadFile(persistenceFile)
	if err != nil {
		log.Printf("[ERROR] Reading persistence file: %v", err)
		return
	}

	mu.Lock()
	defer mu.Unlock()
	if err := json.Unmarshal(data, &queues); err != nil {
		log.Printf("[ERROR] Unmarshaling persistence data: %v", err)
		return
	}
	log.Printf("[INFO] Loaded %d queues from persistence", len(queues))
}

func saveQueues() {
	mu.Lock()
	data, err := json.MarshalIndent(queues, "", "  ")
	mu.Unlock()

	if err != nil {
		log.Printf("[ERROR] Marshaling queues: %v", err)
		return
	}

	if err := ioutil.WriteFile(persistenceFile, data, 0644); err != nil {
		log.Printf("[ERROR] Writing persistence file: %v", err)
	}
}

func getQueueName(path string) string {
	name := strings.TrimPrefix(path, "/")
	name = strings.TrimSpace(name)
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
		log.Printf("[WARN] Invalid JSON from %s: %v", req.RemoteAddr, err)
		http.Error(rw, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	if err := m.Validate(); err != nil {
		log.Printf("[WARN] Validation failed from %s: %v", req.RemoteAddr, err)
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}

	handlePublish(queueName, m)

	rw.WriteHeader(http.StatusCreated)
	log.Printf("[INFO][HTTP][%s] Published message %s", queueName, m.ID)
}

func handlePublish(queueName string, m message) {
	mu.Lock()
	// Fan-out: Send to all active subscribers
	subs := subscribers[queueName]
	if len(subs) > 0 {
		for _, ch := range subs {
			// Non-blocking send to avoid hanging publisher if a sub is slow
			select {
			case ch <- m:
			default:
				log.Printf("[WARN][%s] Subscriber channel full, dropping message for one sub", queueName)
			}
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
		m := q[0]
		queues[queueName] = q[1:]
		currentLen := len(queues[queueName])
		mu.Unlock()

		saveQueues()
		deliverMessage(rw, m, queueName, currentLen)
		return
	}

	// No messages in queue, wait for a new one
	ch := make(chan message, 1)
	subscribers[queueName] = append(subscribers[queueName], ch)
	mu.Unlock()

	log.Printf("[INFO][HTTP][%s] Subscriber waiting (long polling)...", queueName)

	select {
	case m := <-ch:
		deliverMessage(rw, m, queueName, 0)
	case <-req.Context().Done():
		// Client disconnected
		mu.Lock()
		subs := subscribers[queueName]
		for i, s := range subs {
			if s == ch {
				subscribers[queueName] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
		mu.Unlock()
		log.Printf("[INFO][HTTP][%s] Subscriber disconnected", queueName)
	}
}

func deliverMessage(rw http.ResponseWriter, m message, queueName string, currentLen int) {
	mJSON, err := json.Marshal(m)
	if err != nil {
		log.Printf("[ERROR] Marshaling message: %v", err)
		http.Error(rw, "Internal server error", http.StatusInternalServerError)
		return
	}

	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	rw.Write(mJSON)

	log.Printf("[INFO][HTTP][%s] Delivered message %s. Queue size: %d", queueName, m.ID, currentLen)
}

func startSocketServer() {
	ln, err := net.Listen("tcp", tcpPort)
	if err != nil {
		log.Fatalf("[FATAL] Socket server failed: %v", err)
	}
	log.Printf("[INFO] Socket server listening on %s", tcpPort)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("[ERROR] Socket accept: %v", err)
			continue
		}
		go handleSocketConn(conn)
	}
}

func handleSocketConn(conn net.Conn) {
	defer conn.Close()
	log.Printf("[INFO][TCP] New connection from %s", conn.RemoteAddr())

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		var action socketAction
		if err := json.Unmarshal(scanner.Bytes(), &action); err != nil {
			log.Printf("[WARN][TCP] Invalid JSON from %s: %v", conn.RemoteAddr(), err)
			fmt.Fprintf(conn, `{"error": "Invalid JSON"}`+"\n")
			continue
		}

		if action.Queue == "" {
			action.Queue = "default"
		}

		switch action.Action {
		case "publish":
			if err := action.Msg.Validate(); err != nil {
				fmt.Fprintf(conn, `{"error": "%s"}`+"\n", err.Error())
				continue
			}
			handlePublish(action.Queue, action.Msg)
			fmt.Fprintf(conn, `{"status": "published", "id": "%s"}`+"\n", action.Msg.ID)
			log.Printf("[INFO][TCP][%s] Published message %s", action.Queue, action.Msg.ID)

		case "subscribe":
			log.Printf("[INFO][TCP][%s] Subscriber waiting...", action.Queue)
			handleSocketSubscribe(conn, action.Queue)
			return

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
		log.Printf("[INFO][TCP][%s] Delivered message %s from queue", queueName, m.ID)
		return
	}

	ch := make(chan message, 1)
	subscribers[queueName] = append(subscribers[queueName], ch)
	mu.Unlock()

	select {
	case m := <-ch:
		mJSON, _ := json.Marshal(m)
		fmt.Fprintf(conn, string(mJSON)+"\n")
		log.Printf("[INFO][TCP][%s] Delivered message %s (fan-out)", queueName, m.ID)
	case <-time.After(30 * time.Second): // Optional timeout for socket sub
		fmt.Fprintf(conn, `{"status": "timeout"}`+"\n")
	}
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
		log.Printf("[INFO] Publisher HTTP server starting on :8001")
		if err := http.ListenAndServe(":8001", publishersServer); err != nil {
			log.Fatalf("[FATAL] Publisher server: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		log.Printf("[INFO] Subscriber HTTP server starting on :8002")
		if err := http.ListenAndServe(":8002", subscribersServer); err != nil {
			log.Fatalf("[FATAL] Subscriber server: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		startSocketServer()
	}()

	wg.Wait()
}
