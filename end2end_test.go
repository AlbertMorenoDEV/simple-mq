package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/AlbertMorenoDEV/simple-mq/internal/broker"
	"github.com/AlbertMorenoDEV/simple-mq/internal/model"
	"github.com/AlbertMorenoDEV/simple-mq/internal/storage"
	httptransport "github.com/AlbertMorenoDEV/simple-mq/internal/transport/http"
)

type testEnv struct {
	broker *broker.Broker
	pubSrv *httptest.Server
	subSrv *httptest.Server
}

func newTestEnv(t *testing.T, store storage.Store) *testEnv {
	t.Helper()
	b, err := broker.New(broker.DefaultOptions(), store, nil)
	if err != nil {
		t.Fatalf("new broker: %v", err)
	}
	opts := httptransport.Options{
		DefaultPollTimeout: 5 * time.Second,
	}
	pub := httptest.NewServer(httptransport.NewPublisherHandler(b, opts))
	sub := httptest.NewServer(httptransport.NewSubscriberHandler(b, opts))
	t.Cleanup(func() {
		pub.Close()
		sub.Close()
		_ = b.Close()
	})
	return &testEnv{broker: b, pubSrv: pub, subSrv: sub}
}

func createSampleMessage() model.Message {
	return model.Message{
		ID:   model.NewUUID(),
		Type: "order.created",
		Data: json.RawMessage(`"hello simple-mq"`),
	}
}

func postMessage(t *testing.T, baseURL, path string, msg model.Message) {
	t.Helper()
	body, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal msg: %v", err)
	}
	resp, err := http.Post(baseURL+path, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST %s: expected 201, got %d", path, resp.StatusCode)
	}
}

func getMessage(t *testing.T, baseURL, path string) model.Message {
	t.Helper()
	resp, err := http.Get(baseURL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: expected 200, got %d", path, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("GET %s: expected application/json, got %q", path, ct)
	}
	var m model.Message
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return m
}

func TestInitialState(t *testing.T) {
	env := newTestEnv(t, storage.NewMemoryStore())

	// On an empty queue, long-polling waits until a message arrives or the client cancels.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, env.subSrv.URL+"/", nil)
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

	// Verify the disconnected subscriber is cleaned up promptly.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if env.broker.ActiveSubscriberCount("default") == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected subscriber to be removed after disconnect")
}

func TestPublishSingleMessage(t *testing.T) {
	env := newTestEnv(t, storage.NewMemoryStore())
	want := createSampleMessage()

	postMessage(t, env.pubSrv.URL, "/", want)
	got := getMessage(t, env.subSrv.URL, "/")

	if got.ID != want.ID || got.Type != want.Type || string(got.Data) != string(want.Data) {
		t.Fatalf("mismatch: want %+v, got %+v", want, got)
	}
}

func TestMultipleQueues(t *testing.T) {
	env := newTestEnv(t, storage.NewMemoryStore())
	msg1 := createSampleMessage()
	msg2 := createSampleMessage()

	postMessage(t, env.pubSrv.URL, "/", msg1)
	postMessage(t, env.pubSrv.URL, "/tasks", msg2)

	got1 := getMessage(t, env.subSrv.URL, "/")
	got2 := getMessage(t, env.subSrv.URL, "/tasks")

	if got1.ID != msg1.ID {
		t.Fatalf("default queue mismatch: want %s, got %s", msg1.ID, got1.ID)
	}
	if got2.ID != msg2.ID {
		t.Fatalf("tasks queue mismatch: want %s, got %s", msg2.ID, got2.ID)
	}
}

func TestFanOut(t *testing.T) {
	env := newTestEnv(t, storage.NewMemoryStore())
	want := createSampleMessage()

	var wg sync.WaitGroup
	wg.Add(2)

	results := make(chan model.Message, 2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			results <- getMessage(t, env.subSrv.URL, "/fanout")
		}()
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && env.broker.ActiveSubscriberCount("fanout") < 2 {
		time.Sleep(10 * time.Millisecond)
	}

	postMessage(t, env.pubSrv.URL, "/fanout", want)
	wg.Wait()
	close(results)

	count := 0
	for got := range results {
		count++
		if got.ID != want.ID {
			t.Fatalf("fan-out subscriber received wrong ID: want %s, got %s", want.ID, got.ID)
		}
	}
	if count != 2 {
		t.Fatalf("expected 2 fan-out deliveries, got %d", count)
	}
}
