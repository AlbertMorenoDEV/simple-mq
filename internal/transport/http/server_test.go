package httptransport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AlbertMorenoDEV/simple-mq/internal/broker"
	"github.com/AlbertMorenoDEV/simple-mq/internal/model"
	"github.com/AlbertMorenoDEV/simple-mq/internal/storage"
)

func setupHTTPTest(t *testing.T, opts Options) (*broker.Broker, *httptest.Server, *httptest.Server) {
	t.Helper()
	b, err := broker.New(broker.DefaultOptions(), storage.NewMemoryStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	pub := httptest.NewServer(NewPublisherHandler(b, opts))
	sub := httptest.NewServer(NewSubscriberHandler(b, opts))
	t.Cleanup(func() {
		pub.Close()
		sub.Close()
		_ = b.Close()
	})
	return b, pub, sub
}

func TestHTTPValidationBodyLimitAndAuth(t *testing.T) {
	_, pub, sub := setupHTTPTest(t, Options{
		MaxBodyBytes: 128,
		AuthToken:    "secret-token",
	})

	// Health check bypasses auth.
	resp, err := http.Get(pub.URL + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz failed: resp=%v err=%v", resp, err)
	}
	resp.Body.Close()

	// Unauthenticated publish -> 401.
	resp, err = http.Post(pub.URL+"/q1", "application/json", strings.NewReader(`{"type":"t"}`))
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %v err=%v", resp, err)
	}
	resp.Body.Close()

	// Helper for authenticated requests.
	doAuth := func(method, url, body string) *http.Response {
		req, err := http.NewRequest(method, url, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer secret-token")
		req.Header.Set("Content-Type", "application/json")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	// Unknown JSON field -> 400.
	r := doAuth(http.MethodPost, pub.URL+"/q1", `{"type":"t","unknown":123}`)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown field, got %d", r.StatusCode)
	}
	r.Body.Close()

	// Trailing garbage -> 400.
	r = doAuth(http.MethodPost, pub.URL+"/q1", `{"type":"t"} {"extra":true}`)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for trailing tokens, got %d", r.StatusCode)
	}
	r.Body.Close()

	// Oversized body -> 413.
	big := `{"type":"t","data":"` + strings.Repeat("a", 200) + `"}`
	r = doAuth(http.MethodPost, pub.URL+"/q1", big)
	if r.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for oversized body, got %d", r.StatusCode)
	}
	r.Body.Close()

	// Non-blocking consume on empty queue -> 204.
	r = doAuth(http.MethodGet, sub.URL+"/q1?timeout=0", "")
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 on empty non-blocking poll, got %d", r.StatusCode)
	}
	r.Body.Close()
}

func TestHTTPBatchAckAdminAndMetrics(t *testing.T) {
	_, pub, sub := setupHTTPTest(t, Options{})

	// Batch publish.
	batchBody := `[{"id":"1","type":"job","data":{"n":1}},{"id":"2","type":"job","data":{"n":2}}]`
	resp, err := http.Post(pub.URL+"/jobs/batch", "application/json", strings.NewReader(batchBody))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("batch publish failed: resp=%v err=%v", resp, err)
	}
	resp.Body.Close()

	// Admin peek & stats.
	resp, err = http.Get(pub.URL + "/queues/jobs/peek?limit=5")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("peek failed: %v %v", resp, err)
	}
	var peeked []model.Message
	if err := json.NewDecoder(resp.Body).Decode(&peeked); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(peeked) != 2 {
		t.Fatalf("expected 2 peeked messages, got %d", len(peeked))
	}

	resp, err = http.Get(pub.URL + "/queues/jobs/stats")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("stats failed: %v %v", resp, err)
	}
	var info broker.QueueInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if info.Depth != 2 {
		t.Fatalf("expected depth 2, got %+v", info)
	}

	// Consume with ack=true and acknowledge via subscriber port.
	resp, err = http.Get(sub.URL + "/jobs?ack=true&timeout=1s")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("consume with ack failed: %v %v", resp, err)
	}
	receipt := resp.Header.Get("X-Message-Receipt")
	var msg model.Message
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if receipt == "" || receipt != msg.Receipt {
		t.Fatalf("expected matching receipt header & field, header=%q field=%q", receipt, msg.Receipt)
	}

	ackResp, err := http.Post(sub.URL+"/jobs/ack/"+receipt, "application/json", nil)
	if err != nil || ackResp.StatusCode != http.StatusOK {
		t.Fatalf("ack failed: %v %v", ackResp, err)
	}
	ackResp.Body.Close()

	// Prometheus metrics check.
	mResp, err := http.Get(pub.URL + "/metrics")
	if err != nil || mResp.StatusCode != http.StatusOK {
		t.Fatalf("metrics failed: %v %v", mResp, err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(mResp.Body)
	mResp.Body.Close()
	if !strings.Contains(buf.String(), "simplemq_published_total 2") || !strings.Contains(buf.String(), "simplemq_acked_total 1") {
		t.Fatalf("unexpected metrics output:\n%s", buf.String())
	}

	// Purge remaining messages.
	delReq, _ := http.NewRequest(http.MethodDelete, pub.URL+"/queues/jobs", nil)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil || delResp.StatusCode != http.StatusOK {
		t.Fatalf("purge failed: %v %v", delResp, err)
	}
	delResp.Body.Close()
}

func TestHTTPSSEStream(t *testing.T) {
	b, pub, sub := setupHTTPTest(t, Options{SSEKeepalive: 5 * time.Second})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sub.URL+"/live/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("expected text/event-stream, got %q", ct)
	}

	// Wait until subscriber is attached.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && b.ActiveSubscriberCount("live") < 1 {
		time.Sleep(10 * time.Millisecond)
	}

	// Publish a message.
	pResp, err := http.Post(pub.URL+"/live", "application/json", strings.NewReader(`{"id":"sse-1","type":"ping","data":"hello"}`))
	if err != nil || pResp.StatusCode != http.StatusCreated {
		t.Fatalf("publish failed: %v %v", pResp, err)
	}
	pResp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	foundData := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			var got model.Message
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &got); err != nil {
				t.Fatalf("invalid SSE data JSON: %v", err)
			}
			if got.ID != "sse-1" {
				t.Fatalf("expected sse-1, got %s", got.ID)
			}
			foundData = true
			break
		}
	}
	if !foundData {
		t.Fatalf("did not receive SSE data frame")
	}
}
