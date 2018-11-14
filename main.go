package main

import (
	"encoding/json"
	"log"
	"net/http"
)

type message struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data string `json:"data"`
	// CreatedAt time.Time
}

var queue []message

func publishersHandler(rw http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" || req.Method != "POST" {
		http.Error(rw, "404 not found.", http.StatusNotFound)
		return
	}

	var m message

	decoder := json.NewDecoder(req.Body)
	err := decoder.Decode(&m)
	if err != nil {
		panic(err)
	}

	queue = append(queue, m)

	rw.WriteHeader(http.StatusCreated)

	log.Printf("New message '%s'. Total '%d' messages in the queue", messageToJSON(m), len(queue))
}

func subscribersHandler(rw http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" || req.Method != "GET" {
		http.Error(rw, "404 not found.", http.StatusNotFound)
		return
	}

	if len(queue) < 1 {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusNoContent)
		rw.Write([]byte(nil))
		return
	}

	var m message

	m, queue = queue[0], queue[1:]

	mJSON := messageToJSON(m)

	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	rw.Write(mJSON)

	log.Printf("Message delivered '%s'. Total '%d' messages in the queue", mJSON, len(queue))
}

func messageToJSON(m message) []byte {
	mJSON, err := json.Marshal(m)
	if err != nil {
		log.Fatal(err)
	}

	return mJSON
}

func main() {
	finish := make(chan bool)

	publishersServer := http.NewServeMux()
	publishersServer.HandleFunc("/", publishersHandler)

	subscribersServer := http.NewServeMux()
	subscribersServer.HandleFunc("/", subscribersHandler)

	go func() {
		if err := http.ListenAndServe(":8001", publishersServer); err != nil {
			log.Fatal(err)
		}
	}()

	go func() {
		if err := http.ListenAndServe(":8002", subscribersServer); err != nil {
			log.Fatal(err)
		}
	}()

	<-finish
}
