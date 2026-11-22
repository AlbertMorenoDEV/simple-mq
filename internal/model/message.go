package model

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	DefaultQueueName = "default"
	MaxQueueNameLen  = 128
)

var (
	ErrInvalidQueueName = errors.New("invalid queue name: must be 1-128 chars of [a-zA-Z0-9._-]")
	ErrMissingType      = errors.New("message type is required")
	ErrInvalidTTL       = errors.New("invalid ttl duration")
	ErrInvalidDelay     = errors.New("invalid delay duration")
)

// Message represents a single queued item.
// Data is a json.RawMessage so callers may pass either a JSON string (backwards
// compatible with v0.1/v0.2) or any valid JSON object, array, number, or bool.
type Message struct {
	ID            string            `json:"id"`
	Type          string            `json:"type"`
	Data          json.RawMessage   `json:"data,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	Timestamp     time.Time         `json:"timestamp,omitempty"`
	TTL           string            `json:"ttl,omitempty"`
	ExpiresAt     *time.Time        `json:"expires_at,omitempty"`
	Delay         string            `json:"delay,omitempty"`
	DeliverAt     *time.Time        `json:"deliver_at,omitempty"`
	DeliveryCount int               `json:"delivery_count,omitempty"`
	Receipt       string            `json:"receipt,omitempty"`
}

// NewUUID generates an RFC 4122 version 4 UUID using crypto/rand.
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fallback to timestamp if entropy source fails unexpectedly.
		return fmt.Sprintf("msg-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // Version 4
	b[8] = (b[8] & 0x3f) | 0x80 // Variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// NormalizeAndValidate populates default fields (ID, Timestamp, ExpiresAt, DeliverAt)
// and validates message constraints.
func (m *Message) NormalizeAndValidate(now time.Time) error {
	m.ID = strings.TrimSpace(m.ID)
	if m.ID == "" {
		m.ID = NewUUID()
	}

	m.Type = strings.TrimSpace(m.Type)
	if m.Type == "" {
		return ErrMissingType
	}

	if m.Timestamp.IsZero() {
		m.Timestamp = now.UTC()
	}

	if m.TTL != "" && m.ExpiresAt == nil {
		d, err := time.ParseDuration(m.TTL)
		if err != nil || d <= 0 {
			return fmt.Errorf("%w: %q", ErrInvalidTTL, m.TTL)
		}
		exp := now.UTC().Add(d)
		m.ExpiresAt = &exp
	}

	if m.Delay != "" && m.DeliverAt == nil {
		d, err := time.ParseDuration(m.Delay)
		if err != nil || d < 0 {
			return fmt.Errorf("%w: %q", ErrInvalidDelay, m.Delay)
		}
		if d > 0 {
			del := now.UTC().Add(d)
			m.DeliverAt = &del
		}
	}

	// Copy Data bytes defensively if present.
	if len(m.Data) > 0 {
		cp := make(json.RawMessage, len(m.Data))
		copy(cp, m.Data)
		m.Data = cp
	}

	return nil
}

// IsExpired reports whether the message has passed its ExpiresAt deadline.
func (m Message) IsExpired(now time.Time) bool {
	return m.ExpiresAt != nil && !now.Before(*m.ExpiresAt)
}

// IsDelayed reports whether the message is scheduled for future delivery.
func (m Message) IsDelayed(now time.Time) bool {
	return m.DeliverAt != nil && now.Before(*m.DeliverAt)
}

// Clone returns a deep copy of the message so concurrent fan-out subscribers
// cannot mutate shared headers or raw payload slices.
func (m Message) Clone() Message {
	cp := m
	if len(m.Data) > 0 {
		cp.Data = make(json.RawMessage, len(m.Data))
		copy(cp.Data, m.Data)
	}
	if len(m.Headers) > 0 {
		cp.Headers = make(map[string]string, len(m.Headers))
		for k, v := range m.Headers {
			cp.Headers[k] = v
		}
	}
	if m.ExpiresAt != nil {
		t := *m.ExpiresAt
		cp.ExpiresAt = &t
	}
	if m.DeliverAt != nil {
		t := *m.DeliverAt
		cp.DeliverAt = &t
	}
	return cp
}

// ValidateQueueName normalizes and validates a queue name.
// An empty name resolves to "default".
func ValidateQueueName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return DefaultQueueName, nil
	}
	if len(name) > MaxQueueNameLen {
		return "", ErrInvalidQueueName
	}
	if name == "." || name == ".." || strings.Contains(name, "..") {
		return "", ErrInvalidQueueName
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		isAlphaNum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		isAllowedPunct := c == '.' || c == '_' || c == '-'
		if !isAlphaNum && !isAllowedPunct {
			return "", ErrInvalidQueueName
		}
	}
	return name, nil
}

// Snapshot represents a point-in-time serializable view of broker state.
type Snapshot struct {
	Queues  map[string][]Message `json:"queues"`
	Delayed map[string][]Message `json:"delayed,omitempty"`
}
