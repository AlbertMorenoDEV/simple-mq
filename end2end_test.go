package main

import (
	"net/http"
	"testing"

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

func TestPublishMessage(t *testing.T) {
	message := map[string]string{"id": "1", "type": "test", "data": "hello"}

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
