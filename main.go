package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
)

type message struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data string `json:"data"`
}

var (
	queue []message
	mu    sync.Mutex
)

func publishersHandler(rw http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" || req.Method != "POST" {
		http.Error(rw, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var m message
	decoder := json.NewDecoder(req.Body)
	if err := decoder.Decode(&m); err != nil {
		http.Error(rw, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	mu.Lock()
	queue = append(queue, m)
	currentLen := len(queue)
	mu.Unlock()

	rw.WriteHeader(http.StatusCreated)

	mJSON, _ := json.Marshal(m)
	log.Printf("New message '%s'. Total '%d' messages in the queue", mJSON, currentLen)
}

func subscribersHandler(rw http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" || req.Method != "GET" {
		http.Error(rw, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	mu.Lock()
	if len(queue) < 1 {
		mu.Unlock()
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusNoContent)
		return
	}

	m := queue[0]
	queue = queue[1:]
	currentLen := len(queue)
	mu.Unlock()

	mJSON, err := json.Marshal(m)
	if err != nil {
		http.Error(rw, "Internal server error", http.StatusInternalServerError)
		return
	}

	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	rw.Write(mJSON)

	log.Printf("Message delivered '%s'. Total '%d' messages in the queue", mJSON, currentLen)
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
