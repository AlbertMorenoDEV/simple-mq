package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
)

type message struct {
	ID   string
	Type string
	Data string
}

var queue []message

func publisher(rw http.ResponseWriter, req *http.Request) {
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

	queue = append(queue, m)

	log.Println(queue)
}

func main() {
	http.HandleFunc("/", publisher)

	fmt.Printf("Starting publisher server...\n")
	if err := http.ListenAndServe(":9000", nil); err != nil {
		log.Fatal(err)
	}
}
