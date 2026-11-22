package model

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMessageNormalizeValidateAndClone(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	// Missing type fails.
	m := Message{}
	if err := m.NormalizeAndValidate(now); !errors.Is(err, ErrMissingType) {
		t.Fatalf("expected ErrMissingType, got %v", err)
	}

	// Invalid TTL / Delay fail.
	m = Message{Type: "t", TTL: "-5s"}
	if err := m.NormalizeAndValidate(now); !errors.Is(err, ErrInvalidTTL) {
		t.Fatalf("expected ErrInvalidTTL, got %v", err)
	}
	m = Message{Type: "t", Delay: "not-a-duration"}
	if err := m.NormalizeAndValidate(now); !errors.Is(err, ErrInvalidDelay) {
		t.Fatalf("expected ErrInvalidDelay, got %v", err)
	}

	// Valid message gets auto-ID, Timestamp, ExpiresAt, DeliverAt, and deep Clone.
	m = Message{
		Type:    "order",
		Data:    json.RawMessage(`{"k":"v"}`),
		Headers: map[string]string{"x-trace": "abc"},
		TTL:     "10s",
		Delay:   "2s",
	}
	if err := m.NormalizeAndValidate(now); err != nil {
		t.Fatalf("NormalizeAndValidate: %v", err)
	}
	if m.ID == "" || m.ExpiresAt == nil || m.DeliverAt == nil {
		t.Fatalf("expected populated metadata: %+v", m)
	}
	if !m.IsDelayed(now.Add(time.Second)) || m.IsDelayed(now.Add(3*time.Second)) {
		t.Fatalf("unexpected IsDelayed result")
	}
	if m.IsExpired(now.Add(5*time.Second)) || !m.IsExpired(now.Add(11*time.Second)) {
		t.Fatalf("unexpected IsExpired result")
	}

	cp := m.Clone()
	cp.Headers["x-trace"] = "mutated"
	if m.Headers["x-trace"] != "abc" {
		t.Fatalf("Clone did not deep-copy headers")
	}
}

func TestValidateQueueName(t *testing.T) {
	if q, err := ValidateQueueName(""); err != nil || q != DefaultQueueName {
		t.Fatalf("empty should resolve to default: q=%q err=%v", q, err)
	}
	if q, err := ValidateQueueName("orders.v1_prod-eu"); err != nil || q != "orders.v1_prod-eu" {
		t.Fatalf("valid queue rejected: q=%q err=%v", q, err)
	}
	for _, bad := range []string{"..", ".", "a..b", "a/b", "a b", strings.Repeat("x", MaxQueueNameLen+1)} {
		if _, err := ValidateQueueName(bad); !errors.Is(err, ErrInvalidQueueName) {
			t.Fatalf("expected ErrInvalidQueueName for %q, got %v", bad, err)
		}
	}
}
