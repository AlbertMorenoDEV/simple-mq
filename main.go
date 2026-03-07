package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
)

type message struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data string `json:"data"`
}

var (
	// queues stores messages keyed by queue name
	queues = make(map[string][]message)
	mu     sync.Mutex
)

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
	queues[queueName] = append(queues[queueName], m)
	currentLen := len(queues[queueName])
	mu.Unlock()

	rw.WriteHeader(http.StatusCreated)

	mJSON, _ := json.Marshal(m)
	log.Printf("[%s] New message '%s'. Total '%d' messages", queueName, mJSON, currentLen)
}

func subscribersHandler(rw http.ResponseWriter, req *http.Request) {
	if req.Method != "GET" {
		http.Error(rw, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	queueName := getQueueName(req.URL.Path)

	mu.Lock()
	q, ok := queues[queueName]
	if !ok || len(q) < 1 {
		mu.Unlock()
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusNoContent)
		return
	}

	m := q[0]
	queues[queueName] = q[1:]
	currentLen := len(queues[queueName])
	mu.Unlock()

	mJSON, err := json.Marshal(m)
	if err != nil {
		http.Error(rw, "Internal server error", http.StatusInternalServerError)
		return
	}

	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	rw.Write(mJSON)

	log.Printf("[%s] Message delivered '%s'. Total '%d' messages", queueName, mJSON, currentLen)
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
