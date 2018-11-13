package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

type message struct {
	ID        string
	Type      string
	Data      string
	CreatedAt time.Time
}

var queue []message

func publishersHandler(rw http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" || req.Method != "POST" {
		http.Error(rw, "404 not found.", http.StatusNotFound)
		return
	}

	req.ParseForm()

	var m message
	for key := range req.Form {

		err := json.Unmarshal([]byte(key), &m)
		if err != nil {
			log.Println(err.Error())
		}
	}

	m.CreatedAt = time.Now().Local()

	queue = append(queue, m)

	log.Println(queue)
	rw.WriteHeader(http.StatusCreated)
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

	var currentItem message

	currentItem, queue = queue[0], queue[1:]

	currentItemJSON, err := json.Marshal(currentItem)
	if err != nil {
		panic(err)
	}

	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	rw.Write(currentItemJSON)
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
