package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/icrowley/fake"
	uuid "github.com/satori/go.uuid"
	"gopkg.in/h2non/baloo.v3"
)

var publishersServerTest = baloo.New("http://localhost:8001")
var subscribersServerTest = baloo.New("http://localhost:8002")

func TestInitialState(t *testing.T) {
	subscribersServerTest.Get("/").
		Expect(t).
		Status(http.StatusNoContent).
		Header("Content-Type", "application/json").
		Type("json").
		JSON(nil).
		Done()
}

func TestPublishSingleMessage(t *testing.T) {
	message := createRandomMessage()

	publishersServerTest.Post("/").
		JSON(message).
		Expect(t).
		Status(http.StatusCreated).
		Done()

	subscribersServerTest.Get("/").
		Expect(t).
		Status(http.StatusOK).
		Header("Content-Type", "application/json").
		Type("json").
		JSON(message).
		Done()
}

func TestMultipleQueues(t *testing.T) {
	msg1 := createRandomMessage()
	msg2 := createRandomMessage()

	// Publish msg1 to default queue
	publishersServerTest.Post("/").
		JSON(msg1).
		Expect(t).
		Status(http.StatusCreated).
		Done()

	// Publish msg2 to named queue 'tasks'
	publishersServerTest.Post("/tasks").
		JSON(msg2).
		Expect(t).
		Status(http.StatusCreated).
		Done()

	// Get from default queue
	subscribersServerTest.Get("/").
		Expect(t).
		Status(http.StatusOK).
		JSON(msg1).
		Done()

	// Get from 'tasks' queue
	subscribersServerTest.Get("/tasks").
		Expect(t).
		Status(http.StatusOK).
		JSON(msg2).
		Done()
}

func createRandomMessage() map[string]string {
	id := uuid.NewV4().String()
	t := fake.Product()
	d, _ := json.Marshal(map[string]string{
		"name":      fake.FirstName(),
		"full_name": fake.FullName(),
	})

	return map[string]string{"id": id, "type": t, "data": string(d)}
}
