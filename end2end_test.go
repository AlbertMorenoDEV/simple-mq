package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/icrowley/fake"
	"gopkg.in/h2non/baloo.v3"
)

var (
	publishersServerTest  *baloo.Client
	subscribersServerTest *baloo.Client
	subscribersServerURL  string
)

func TestMain(m *testing.M) {
	pubServer := httptest.NewServer(http.HandlerFunc(publishersHandler))
	subServer := httptest.NewServer(http.HandlerFunc(subscribersHandler))

	publishersServerTest = baloo.New(pubServer.URL)
	subscribersServerTest = baloo.New(subServer.URL)
	subscribersServerURL = subServer.URL

	code := m.Run()

	pubServer.Close()
	subServer.Close()
	_ = os.Remove(persistenceFile)
	os.Exit(code)
}

func resetState(t *testing.T) {
	t.Helper()
	mu.Lock()
	queues = make(map[string][]message)
	subscribers = make(map[string][]chan message)
	mu.Unlock()
	_ = os.Remove(persistenceFile)
	t.Cleanup(func() {
		_ = os.Remove(persistenceFile)
	})
}

func TestInitialState(t *testing.T) {
	resetState(t)

	// On an empty queue, subscribersHandler long-polls until a message arrives
	// or the client disconnects.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, subscribersServerURL+"/", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected long-polling request to time out with DeadlineExceeded, got err=%v", err)
	}

	// Verify the disconnected subscriber is cleaned up.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		remaining := len(subscribers["default"])
		mu.Unlock()
		if remaining == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected subscriber to be removed after disconnect")
}

func TestPublishSingleMessage(t *testing.T) {
	resetState(t)
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
	resetState(t)
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

func TestFanOut(t *testing.T) {
	resetState(t)
	msg := createRandomMessage()
	wg := sync.WaitGroup{}
	wg.Add(2)

	// Start two subscribers waiting for a message
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			baloo.New(subscribersServerURL).Get("/fanout").
				Expect(t).
				Status(http.StatusOK).
				JSON(msg).
				Done()
		}()
	}

	// Wait until both subscribers are registered
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(subscribers["fanout"])
		mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Publish one message
	publishersServerTest.Post("/fanout").
		JSON(msg).
		Expect(t).
		Status(http.StatusCreated).
		Done()

	wg.Wait()
}

func createRandomMessage() map[string]string {
	id := uuid.NewString()
	t := fake.Product()
	d, _ := json.Marshal(map[string]string{
		"name":      fake.FirstName(),
		"full_name": fake.FullName(),
	})

	return map[string]string{"id": id, "type": t, "data": string(d)}
}
