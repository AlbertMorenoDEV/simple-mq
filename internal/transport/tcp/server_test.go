package tcptransport

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/AlbertMorenoDEV/simple-mq/internal/broker"
	"github.com/AlbertMorenoDEV/simple-mq/internal/model"
	"github.com/AlbertMorenoDEV/simple-mq/internal/storage"
)

func setupTCPTest(t *testing.T, opts Options) (*broker.Broker, string) {
	t.Helper()
	b, err := broker.New(broker.DefaultOptions(), storage.NewMemoryStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(b, opts)
	go func() {
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = b.Close()
	})
	return b, ln.Addr().String()
}

func TestTCPJSONInjectionSafeAndLargePayload(t *testing.T) {
	_, addr := setupTCPTest(t, Options{MaxMessageBytes: 256 * 1024})

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	enc := json.NewEncoder(conn)
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 512*1024)

	// ID containing quotes and newline must produce strictly valid JSON response.
	trickyID := "id\"with\nquotes\\and\\slashes"
	if err := enc.Encode(map[string]any{
		"action": "publish",
		"queue":  "safe",
		"message": map[string]any{
			"id":   trickyID,
			"type": "test",
			"data": strings.Repeat("x", 80_000), // >64KB default bufio.Scanner limit!
		},
	}); err != nil {
		t.Fatal(err)
	}

	if !scanner.Scan() {
		t.Fatalf("expected response line, err=%v", scanner.Err())
	}
	var resp map[string]string
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %s (%v)", scanner.Text(), err)
	}
	if resp["status"] != "published" || resp["id"] != trickyID {
		t.Fatalf("unexpected publish resp: %+v", resp)
	}
}

func TestTCPTimeoutAndDisconnectDoNotLeakOrDropMessages(t *testing.T) {
	b, addr := setupTCPTest(t, Options{DefaultPollTimeout: 80 * time.Millisecond})

	// 1. Timeout must clean up subscriber so subsequent publish is queued, not dropped!
	conn1, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn1.Close()

	_, _ = conn1.Write([]byte(`{"action":"subscribe","queue":"leak-check","timeout":"60ms"}` + "\n"))
	sc1 := bufio.NewScanner(conn1)
	if !sc1.Scan() {
		t.Fatalf("expected timeout response")
	}
	if !strings.Contains(sc1.Text(), `"timeout"`) {
		t.Fatalf("expected timeout JSON, got %s", sc1.Text())
	}
	if got := b.ActiveSubscriberCount("leak-check"); got != 0 {
		t.Fatalf("leaked subscriber after timeout: count=%d", got)
	}

	// Publish after timeout: message must be queued and retrievable!
	if _, err := b.Publish("leak-check", model.Message{ID: "after-timeout", Type: "t"}); err != nil {
		t.Fatal(err)
	}
	m, err := b.Consume(context.Background(), "leak-check", broker.ConsumeOptions{Timeout: 0})
	if err != nil || m.ID != "after-timeout" {
		t.Fatalf("message dropped after TCP timeout! msg=%+v err=%v", m, err)
	}

	// 2. Client disconnect while waiting must immediately remove subscriber.
	conn2, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn2.Write([]byte(`{"action":"subscribe","queue":"disc-check","timeout":"10s"}` + "\n"))

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && b.ActiveSubscriberCount("disc-check") < 1 {
		time.Sleep(10 * time.Millisecond)
	}
	_ = conn2.Close()

	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) && b.ActiveSubscriberCount("disc-check") > 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := b.ActiveSubscriberCount("disc-check"); got != 0 {
		t.Fatalf("subscriber was not cleaned up after TCP disconnect: count=%d", got)
	}
}

func TestTCPStreamingAckAndAuth(t *testing.T) {
	b, addr := setupTCPTest(t, Options{AuthToken: "tcp-secret"})

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	enc := json.NewEncoder(conn)
	sc := bufio.NewScanner(conn)

	// Unauthenticated publish -> error.
	_ = enc.Encode(map[string]any{"action": "publish", "queue": "s", "message": map[string]any{"type": "t"}})
	sc.Scan()
	if !strings.Contains(sc.Text(), "unauthorized") {
		t.Fatalf("expected unauthorized, got %s", sc.Text())
	}

	// Authenticate.
	_ = enc.Encode(map[string]any{"action": "auth", "token": "tcp-secret"})
	sc.Scan()
	if !strings.Contains(sc.Text(), "authenticated") {
		t.Fatalf("expected authenticated, got %s", sc.Text())
	}

	// Start streaming subscription with ACK enabled.
	_ = enc.Encode(map[string]any{"action": "subscribe", "queue": "streamq", "stream": true, "ack": true})
	sc.Scan()
	if !strings.Contains(sc.Text(), "subscribed") {
		t.Fatalf("expected subscribed, got %s", sc.Text())
	}

	for _, wantID := range []string{"s1", "s2"} {
		_, _ = b.Publish("streamq", model.Message{ID: wantID, Type: "ev"})
		if !sc.Scan() {
			t.Fatalf("missing streamed message %s", wantID)
		}
		var m model.Message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("unmarshal streamed msg: %v", err)
		}
		if m.ID != wantID || m.Receipt == "" {
			t.Fatalf("unexpected streamed msg: %+v", m)
		}
		// Send ACK on the same multiplexed connection!
		_ = enc.Encode(map[string]any{"action": "ack", "queue": "streamq", "receipt": m.Receipt})
		if !sc.Scan() {
			t.Fatalf("missing ack response")
		}
		if !strings.Contains(sc.Text(), `"acked"`) {
			t.Fatalf("expected acked response, got %s", sc.Text())
		}
	}

	// Unsubscribe cleanly.
	_ = enc.Encode(map[string]any{"action": "unsubscribe"})
	sc.Scan()
	if !strings.Contains(sc.Text(), "unsubscribed") {
		t.Fatalf("expected unsubscribed, got %s", sc.Text())
	}
}
