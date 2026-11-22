package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/AlbertMorenoDEV/simple-mq/internal/model"
	"github.com/AlbertMorenoDEV/simple-mq/internal/storage"
)

func newTestBroker(t *testing.T, opts Options) *Broker {
	t.Helper()
	b, err := New(opts, storage.NewMemoryStore(), nil)
	if err != nil {
		t.Fatalf("New broker: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestPublishAutoIDArbitraryJSONAndQueueValidation(t *testing.T) {
	b := newTestBroker(t, DefaultOptions())

	// Auto-generated ID + JSON object payload.
	pub, err := b.Publish("orders", model.Message{
		Type: "order.created",
		Data: json.RawMessage(`{"amount":42,"items":["a","b"]}`),
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if pub.ID == "" {
		t.Fatalf("expected auto-generated ID")
	}

	// Invalid queue names should be rejected.
	for _, bad := range []string{"../etc/passwd", "a/b", "has space", "bad?query"} {
		if _, err := b.Publish(bad, model.Message{Type: "x"}); !errors.Is(err, model.ErrInvalidQueueName) {
			t.Fatalf("queue %q: expected ErrInvalidQueueName, got %v", bad, err)
		}
	}

	got, err := b.Consume(context.Background(), "orders", ConsumeOptions{Timeout: 0})
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if got.ID != pub.ID || string(got.Data) != `{"amount":42,"items":["a","b"]}` {
		t.Fatalf("unexpected message: %+v", got)
	}
}

func TestCompetingConsumersModeQueue(t *testing.T) {
	opts := DefaultOptions()
	opts.DeliveryMode = ModeQueue
	b := newTestBroker(t, opts)

	const numSubs = 2
	const numMsgs = 10

	var (
		mu       sync.Mutex
		received = make(map[string]int)
		wg       sync.WaitGroup
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	for i := 0; i < numSubs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				m, err := b.Consume(ctx, "jobs", ConsumeOptions{Timeout: 300 * time.Millisecond})
				if err != nil {
					return
				}
				mu.Lock()
				received[m.ID]++
				mu.Unlock()
			}
		}()
	}

	// Wait for subscribers to attach.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && b.ActiveSubscriberCount("jobs") < numSubs {
		time.Sleep(5 * time.Millisecond)
	}

	for i := 0; i < numMsgs; i++ {
		if _, err := b.Publish("jobs", model.Message{
			ID:   fmt.Sprintf("job-%d", i),
			Type: "work",
		}); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}

	// Wait until all messages are consumed.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n == numMsgs {
			cancel()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	wg.Wait()

	if len(received) != numMsgs {
		t.Fatalf("expected %d unique messages, got %d", numMsgs, len(received))
	}
	for id, count := range received {
		if count != 1 {
			t.Fatalf("message %s delivered %d times in queue mode (expected 1)", id, count)
		}
	}
}

func TestAckNackVisibilityTimeoutAndDLQ(t *testing.T) {
	opts := DefaultOptions()
	opts.VisibilityTimeout = 80 * time.Millisecond
	opts.MaxRetries = 2
	opts.ReapInterval = 20 * time.Millisecond
	b := newTestBroker(t, opts)

	// 1. ACK path
	_, err := b.Publish("tasks", model.Message{ID: "ok-1", Type: "work"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := b.Consume(context.Background(), "tasks", ConsumeOptions{Timeout: 0, AckRequired: true})
	if err != nil || m.Receipt == "" {
		t.Fatalf("expected receipt, got msg=%+v err=%v", m, err)
	}
	if err := b.Ack("tasks", m.Receipt); err != nil {
		t.Fatalf("Ack failed: %v", err)
	}
	if _, err := b.Consume(context.Background(), "tasks", ConsumeOptions{Timeout: 0}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected queue empty after ack, got %v", err)
	}

	// 2. Visibility timeout -> redelivery -> NACK -> DLQ
	_, err = b.Publish("tasks", model.Message{ID: "poison-1", Type: "work"})
	if err != nil {
		t.Fatal(err)
	}

	// Attempt 1: let visibility timeout expire without acking.
	m1, err := b.Consume(context.Background(), "tasks", ConsumeOptions{
		Timeout:           0,
		AckRequired:       true,
		VisibilityTimeout: 60 * time.Millisecond,
	})
	if err != nil || m1.DeliveryCount != 1 {
		t.Fatalf("attempt 1 unexpected: msg=%+v err=%v", m1, err)
	}

	// Attempt 2: wait for reaper to redeliver, then NACK (DeliveryCount==2 == MaxRetries -> DLQ).
	m2, err := b.Consume(context.Background(), "tasks", ConsumeOptions{
		Timeout:     time.Second,
		AckRequired: true,
	})
	if err != nil || m2.ID != "poison-1" || m2.DeliveryCount != 2 {
		t.Fatalf("attempt 2 unexpected: msg=%+v err=%v", m2, err)
	}
	if err := b.Nack("tasks", m2.Receipt); err != nil {
		t.Fatalf("Nack failed: %v", err)
	}

	// Verify message landed in tasks.dlq
	dlqMsg, err := b.Consume(context.Background(), "tasks.dlq", ConsumeOptions{Timeout: 0})
	if err != nil || dlqMsg.ID != "poison-1" {
		t.Fatalf("expected poison-1 in tasks.dlq, got msg=%+v err=%v", dlqMsg, err)
	}

	metrics := b.Metrics()
	if metrics.AckedTotal != 1 || metrics.NackedTotal != 1 || metrics.DeadLetteredTotal != 1 {
		t.Fatalf("unexpected metrics: %+v", metrics)
	}
}

func TestTTLAndDelayedDelivery(t *testing.T) {
	opts := DefaultOptions()
	opts.ReapInterval = 15 * time.Millisecond
	b := newTestBroker(t, opts)

	// Message with short TTL expires before consume.
	if _, err := b.Publish("ephemeral", model.Message{
		ID:   "exp-1",
		Type: "temp",
		TTL:  "40ms",
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if _, err := b.Consume(context.Background(), "ephemeral", ConsumeOptions{Timeout: 0}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected expired message to be skipped, got %v", err)
	}

	// Delayed message is unavailable immediately, then delivered once due.
	if _, err := b.Publish("scheduled", model.Message{
		ID:    "del-1",
		Type:  "reminder",
		Delay: "70ms",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Consume(context.Background(), "scheduled", ConsumeOptions{Timeout: 0}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected delayed message not yet available, got %v", err)
	}
	got, err := b.Consume(context.Background(), "scheduled", ConsumeOptions{Timeout: time.Second})
	if err != nil || got.ID != "del-1" {
		t.Fatalf("expected del-1 after delay, got msg=%+v err=%v", got, err)
	}
}

func TestSubscribeStreamBacklogAndLiveFanOut(t *testing.T) {
	b := newTestBroker(t, DefaultOptions())

	// Pre-populate backlog.
	_, err := b.PublishBatch("events", []model.Message{
		{ID: "b1", Type: "ev"},
		{ID: "b2", Type: "ev"},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, unsub, err := b.SubscribeStream(ctx, "events", ConsumeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	// Publish a live message after subscribing.
	if _, err := b.Publish("events", model.Message{ID: "live-3", Type: "ev"}); err != nil {
		t.Fatal(err)
	}

	want := []string{"b1", "b2", "live-3"}
	for _, w := range want {
		select {
		case m := <-ch:
			if m.ID != w {
				t.Fatalf("want %s, got %s", w, m.ID)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", w)
		}
	}

	unsub()
	if got := b.ActiveSubscriberCount("events"); got != 0 {
		t.Fatalf("expected 0 subscribers after unsub, got %d", got)
	}
}

func TestLimitsAndAdminOperations(t *testing.T) {
	opts := DefaultOptions()
	opts.MaxQueueDepth = 2
	opts.MaxQueues = 2
	b := newTestBroker(t, opts)

	if _, err := b.Publish("q1", model.Message{ID: "1", Type: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Publish("q1", model.Message{ID: "2", Type: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Publish("q1", model.Message{ID: "3", Type: "t"}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("expected ErrQueueFull, got %v", err)
	}

	if _, err := b.Publish("q2", model.Message{ID: "4", Type: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Publish("q3", model.Message{ID: "5", Type: "t"}); !errors.Is(err, ErrMaxQueuesReached) {
		t.Fatalf("expected ErrMaxQueuesReached, got %v", err)
	}

	peeked, err := b.Peek("q1", 10)
	if err != nil || len(peeked) != 2 {
		t.Fatalf("unexpected peek: %+v err=%v", peeked, err)
	}
	if err := b.PurgeQueue("q1"); err != nil {
		t.Fatal(err)
	}
	stats, err := b.QueueStats("q1")
	if err != nil || stats.Depth != 0 {
		t.Fatalf("expected empty q1 after purge, got %+v err=%v", stats, err)
	}
}

func TestWALPersistenceRestartRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queues.json")

	store1, err := storage.NewWALStore(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	b1, err := New(DefaultOptions(), store1, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := b1.Publish("orders", model.Message{ID: "o1", Type: "order"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b1.Publish("orders", model.Message{ID: "o2", Type: "order"}); err != nil {
		t.Fatal(err)
	}
	// Consume o1 in ACK mode without ACKing; restart should preserve o1!
	if _, err := b1.Consume(context.Background(), "orders", ConsumeOptions{Timeout: 0, AckRequired: true}); err != nil {
		t.Fatal(err)
	}
	if err := b1.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen from disk.
	store2, err := storage.NewWALStore(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := New(DefaultOptions(), store2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()

	msgs, err := b2.ConsumeBatch(context.Background(), "orders", 10, ConsumeOptions{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].ID != "o1" || msgs[1].ID != "o2" {
		t.Fatalf("expected unacked o1 and queued o2 to survive restart, got %+v", msgs)
	}
}

func TestConcurrentPublishConsumeRace(t *testing.T) {
	opts := DefaultOptions()
	opts.DeliveryMode = ModeQueue
	b := newTestBroker(t, opts)

	const total = 100
	var wg sync.WaitGroup

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < total/4; j++ {
				_, _ = b.Publish("stress", model.Message{
					ID:   fmt.Sprintf("w%d-%d", worker, j),
					Type: "stress",
				})
			}
		}(i)
	}

	consumed := 0
	var mu sync.Mutex
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				_, err := b.Consume(ctx, "stress", ConsumeOptions{Timeout: 100 * time.Millisecond})
				if err != nil {
					return
				}
				mu.Lock()
				consumed++
				if consumed == total {
					cancel()
				}
				mu.Unlock()
			}
		}()
	}

	wg.Wait()
	if consumed != total {
		t.Fatalf("expected %d consumed messages, got %d", total, consumed)
	}
}
