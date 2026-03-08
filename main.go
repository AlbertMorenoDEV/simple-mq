package main

import (
	"encoding/json"
	"io/ioutil"
	"log"
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

const persistenceFile = "queues.json"

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
	currentLen := len(queues[queueName])
	mu.Unlock()

	saveQueues()

	rw.WriteHeader(http.StatusCreated)

	mJSON, _ := json.Marshal(m)
	log.Printf("[%s] New message '%s'. Total '%d' messages in queue", queueName, mJSON, currentLen)
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

	log.Printf("[%s] Message delivered '%s'. Remaining in queue: %d", queueName, mJSON, currentLen)
}

func main() {
	publishersServer := http.NewServeMux()
	publishersServer.HandleFunc("/", publishersHandler)

	subscribersServer := http.NewServeMux()
	subscribersServer.HandleFunc("/", subscribersHandler)

	wg := sync.WaitGroup{}
	wg.Add(2)

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

	wg.Wait()
}
