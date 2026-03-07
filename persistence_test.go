package main

import (
	"net/http"
	"os"
	"testing"
)

func TestPersistence(t *testing.T) {
	// Clean up before test
	os.Remove(persistenceFile)
	defer os.Remove(persistenceFile)

	msg := createRandomMessage()

	// 1. Publish to default queue
	publishersServerTest.Post("/").
		JSON(msg).
		Expect(t).
		Status(http.StatusCreated).
		Done()

	// 2. Simulate restart by clearing memory and loading from file
	mu.Lock()
	queues = make(map[string][]message)
	mu.Unlock()

	loadQueues()

	// 3. Try to get the message back
	subscribersServerTest.Get("/").
		Expect(t).
		Status(http.StatusOK).
		JSON(msg).
		Done()
}
