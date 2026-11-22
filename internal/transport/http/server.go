package httptransport

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AlbertMorenoDEV/simple-mq/internal/broker"
	"github.com/AlbertMorenoDEV/simple-mq/internal/model"
)

// Options configures the HTTP transport handlers.
type Options struct {
	MaxBodyBytes       int64
	DefaultPollTimeout time.Duration
	SSEKeepalive       time.Duration
	AuthToken          string
	Logger             *slog.Logger
}

func normalizeOptions(opts Options) Options {
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 1 << 20 // 1 MiB
	}
	if opts.DefaultPollTimeout == 0 {
		opts.DefaultPollTimeout = 30 * time.Second
	}
	if opts.SSEKeepalive <= 0 {
		opts.SSEKeepalive = 15 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return opts
}

// NewHTTPServer builds an *http.Server with safe header and idle timeouts.
// WriteTimeout is intentionally left unset so long-polling and SSE streams are
// governed by per-request contexts instead of hard socket cutoffs.
func NewHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

type handlerSet struct {
	broker *broker.Broker
	opts   Options
}

// NewPublisherHandler returns the HTTP handler for the publisher & admin server (:8001).
func NewPublisherHandler(b *broker.Broker, opts Options) http.Handler {
	h := &handlerSet{broker: b, opts: normalizeOptions(opts)}
	return http.HandlerFunc(h.servePublisher)
}

// NewSubscriberHandler returns the HTTP handler for the subscriber server (:8002).
func NewSubscriberHandler(b *broker.Broker, opts Options) http.Handler {
	h := &handlerSet{broker: b, opts: normalizeOptions(opts)}
	return http.HandlerFunc(h.serveSubscriber)
}

func (h *handlerSet) checkAuth(rw http.ResponseWriter, req *http.Request) bool {
	if h.opts.AuthToken == "" {
		return true
	}
	authHeader := req.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		writeJSONError(rw, http.StatusUnauthorized, "unauthorized")
		return false
	}
	token := strings.TrimPrefix(authHeader, prefix)
	if subtle.ConstantTimeCompare([]byte(token), []byte(h.opts.AuthToken)) != 1 {
		writeJSONError(rw, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

func (h *handlerSet) servePublisher(rw http.ResponseWriter, req *http.Request) {
	path := req.URL.Path

	// Health checks do not require auth.
	if req.Method == http.MethodGet && (path == "/healthz" || path == "/readyz") {
		writeJSON(rw, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	if !h.checkAuth(rw, req) {
		return
	}

	// Metrics & Admin endpoints
	if req.Method == http.MethodGet && path == "/metrics" {
		h.handleMetrics(rw)
		return
	}
	if path == "/queues" {
		if req.Method != http.MethodGet {
			writeJSONError(rw, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		writeJSON(rw, http.StatusOK, h.broker.ListQueues())
		return
	}
	if strings.HasPrefix(path, "/queues/") {
		h.handleQueueAdmin(rw, req)
		return
	}

	// Publishing / ACK / NACK endpoints
	if req.Method != http.MethodPost {
		writeJSONError(rw, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	trimmed := strings.TrimPrefix(path, "/")
	parts := splitNonEmpty(trimmed)

	switch len(parts) {
	case 0:
		h.handlePublishSingle(rw, req, model.DefaultQueueName)
	case 1:
		h.handlePublishSingle(rw, req, parts[0])
	case 2:
		if parts[1] == "batch" {
			h.handlePublishBatch(rw, req, parts[0])
			return
		}
		writeJSONError(rw, http.StatusNotFound, "unknown endpoint")
	case 3:
		switch parts[1] {
		case "ack":
			h.handleAck(rw, parts[0], parts[2])
		case "nack":
			h.handleNack(rw, parts[0], parts[2])
		default:
			writeJSONError(rw, http.StatusNotFound, "unknown endpoint")
		}
	default:
		writeJSONError(rw, http.StatusNotFound, "unknown endpoint")
	}
}

func (h *handlerSet) serveSubscriber(rw http.ResponseWriter, req *http.Request) {
	path := req.URL.Path

	if req.Method == http.MethodGet && (path == "/healthz" || path == "/readyz") {
		writeJSON(rw, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	if !h.checkAuth(rw, req) {
		return
	}

	if req.Method == http.MethodGet && path == "/metrics" {
		h.handleMetrics(rw)
		return
	}

	trimmed := strings.TrimPrefix(path, "/")
	parts := splitNonEmpty(trimmed)

	// Allow ACK/NACK directly on the subscriber port as well.
	if req.Method == http.MethodPost && len(parts) == 3 {
		switch parts[1] {
		case "ack":
			h.handleAck(rw, parts[0], parts[2])
			return
		case "nack":
			h.handleNack(rw, parts[0], parts[2])
			return
		}
	}

	if req.Method != http.MethodGet {
		writeJSONError(rw, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	switch len(parts) {
	case 0:
		h.handleConsume(rw, req, model.DefaultQueueName)
	case 1:
		if parts[0] == "stream" {
			h.handleSSEStream(rw, req, model.DefaultQueueName)
			return
		}
		h.handleConsume(rw, req, parts[0])
	case 2:
		if parts[1] == "stream" {
			h.handleSSEStream(rw, req, parts[0])
			return
		}
		writeJSONError(rw, http.StatusNotFound, "unknown endpoint")
	default:
		writeJSONError(rw, http.StatusNotFound, "unknown endpoint")
	}
}

func (h *handlerSet) handleQueueAdmin(rw http.ResponseWriter, req *http.Request) {
	trimmed := strings.TrimPrefix(req.URL.Path, "/queues/")
	parts := splitNonEmpty(trimmed)

	if len(parts) == 1 && req.Method == http.MethodDelete {
		qName, err := model.ValidateQueueName(parts[0])
		if err != nil {
			writeJSONError(rw, http.StatusBadRequest, err.Error())
			return
		}
		if err := h.broker.PurgeQueue(qName); err != nil {
			writeJSONError(rw, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(rw, http.StatusOK, map[string]string{"status": "purged", "queue": qName})
		return
	}

	if len(parts) == 2 && req.Method == http.MethodGet {
		switch parts[1] {
		case "stats":
			info, err := h.broker.QueueStats(parts[0])
			if err != nil {
				writeJSONError(rw, http.StatusBadRequest, err.Error())
				return
			}
			writeJSON(rw, http.StatusOK, info)
			return
		case "peek":
			limit := 10
			if raw := req.URL.Query().Get("limit"); raw != "" {
				n, err := strconv.Atoi(raw)
				if err != nil || n <= 0 || n > 1000 {
					writeJSONError(rw, http.StatusBadRequest, "invalid limit: must be 1-1000")
					return
				}
				limit = n
			}
			msgs, err := h.broker.Peek(parts[0], limit)
			if err != nil {
				writeJSONError(rw, http.StatusBadRequest, err.Error())
				return
			}
			writeJSON(rw, http.StatusOK, msgs)
			return
		}
	}

	writeJSONError(rw, http.StatusNotFound, "unknown admin endpoint")
}

func (h *handlerSet) handlePublishSingle(rw http.ResponseWriter, req *http.Request, rawQueue string) {
	var msg model.Message
	if err := h.decodeStrictJSON(rw, req, &msg); err != nil {
		h.handleDecodeError(rw, req, err)
		return
	}

	published, err := h.broker.Publish(rawQueue, msg)
	if err != nil {
		h.handleBrokerError(rw, err)
		return
	}

	qName, _ := model.ValidateQueueName(rawQueue)
	h.opts.Logger.Info("published message", "transport", "http", "queue", qName, "id", published.ID)
	writeJSON(rw, http.StatusCreated, map[string]string{
		"status": "published",
		"id":     published.ID,
		"queue":  qName,
	})
}

func (h *handlerSet) handlePublishBatch(rw http.ResponseWriter, req *http.Request, rawQueue string) {
	var msgs []model.Message
	if err := h.decodeStrictJSON(rw, req, &msgs); err != nil {
		h.handleDecodeError(rw, req, err)
		return
	}

	published, err := h.broker.PublishBatch(rawQueue, msgs)
	if err != nil {
		h.handleBrokerError(rw, err)
		return
	}

	ids := make([]string, len(published))
	for i, m := range published {
		ids[i] = m.ID
	}
	qName, _ := model.ValidateQueueName(rawQueue)
	h.opts.Logger.Info("published message batch", "transport", "http", "queue", qName, "count", len(published))
	writeJSON(rw, http.StatusCreated, map[string]any{
		"status": "published",
		"queue":  qName,
		"count":  len(published),
		"ids":    ids,
	})
}

func (h *handlerSet) handleConsume(rw http.ResponseWriter, req *http.Request, rawQueue string) {
	consumeOpts, maxBatch, err := h.parseConsumeQuery(req)
	if err != nil {
		writeJSONError(rw, http.StatusBadRequest, err.Error())
		return
	}

	if maxBatch > 1 {
		msgs, err := h.broker.ConsumeBatch(req.Context(), rawQueue, maxBatch, consumeOpts)
		if err != nil {
			h.handleConsumeError(rw, err)
			return
		}
		writeJSON(rw, http.StatusOK, msgs)
		return
	}

	msg, err := h.broker.Consume(req.Context(), rawQueue, consumeOpts)
	if err != nil {
		h.handleConsumeError(rw, err)
		return
	}
	if msg.Receipt != "" {
		rw.Header().Set("X-Message-Receipt", msg.Receipt)
	}
	writeJSON(rw, http.StatusOK, msg)
}

func (h *handlerSet) handleSSEStream(rw http.ResponseWriter, req *http.Request, rawQueue string) {
	flusher, ok := rw.(http.Flusher)
	if !ok {
		writeJSONError(rw, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	consumeOpts, _, err := h.parseConsumeQuery(req)
	if err != nil {
		writeJSONError(rw, http.StatusBadRequest, err.Error())
		return
	}

	ch, unsub, err := h.broker.SubscribeStream(req.Context(), rawQueue, consumeOpts)
	if err != nil {
		h.handleBrokerError(rw, err)
		return
	}
	defer unsub()

	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.Header().Set("Connection", "keep-alive")
	rw.WriteHeader(http.StatusOK)
	flusher.Flush()

	keepalive := time.NewTicker(h.opts.SSEKeepalive)
	defer keepalive.Stop()

	for {
		select {
		case msg, open := <-ch:
			if !open {
				return
			}
			payload, err := json.Marshal(msg)
			if err != nil {
				h.opts.Logger.Error("marshal SSE message failed", "err", err)
				continue
			}
			if _, err := fmt.Fprintf(rw, "id: %s\nevent: message\ndata: %s\n\n", msg.ID, payload); err != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if _, err := io.WriteString(rw, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-req.Context().Done():
			return
		}
	}
}

func (h *handlerSet) handleAck(rw http.ResponseWriter, rawQueue, receipt string) {
	if err := h.broker.Ack(rawQueue, receipt); err != nil {
		h.handleBrokerError(rw, err)
		return
	}
	qName, _ := model.ValidateQueueName(rawQueue)
	writeJSON(rw, http.StatusOK, map[string]string{
		"status":  "acked",
		"queue":   qName,
		"receipt": receipt,
	})
}

func (h *handlerSet) handleNack(rw http.ResponseWriter, rawQueue, receipt string) {
	if err := h.broker.Nack(rawQueue, receipt); err != nil {
		h.handleBrokerError(rw, err)
		return
	}
	qName, _ := model.ValidateQueueName(rawQueue)
	writeJSON(rw, http.StatusOK, map[string]string{
		"status":  "nacked",
		"queue":   qName,
		"receipt": receipt,
	})
}

func (h *handlerSet) handleMetrics(rw http.ResponseWriter) {
	m := h.broker.Metrics()
	rw.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	rw.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(rw,
		"# HELP simplemq_published_total Total number of messages published.\n"+
			"# TYPE simplemq_published_total counter\n"+
			"simplemq_published_total %d\n"+
			"# HELP simplemq_delivered_total Total number of messages delivered.\n"+
			"# TYPE simplemq_delivered_total counter\n"+
			"simplemq_delivered_total %d\n"+
			"# HELP simplemq_acked_total Total number of acknowledged messages.\n"+
			"# TYPE simplemq_acked_total counter\n"+
			"simplemq_acked_total %d\n"+
			"# HELP simplemq_nacked_total Total number of negatively acknowledged messages.\n"+
			"# TYPE simplemq_nacked_total counter\n"+
			"simplemq_nacked_total %d\n"+
			"# HELP simplemq_redelivered_total Total number of redelivered messages.\n"+
			"# TYPE simplemq_redelivered_total counter\n"+
			"simplemq_redelivered_total %d\n"+
			"# HELP simplemq_dead_lettered_total Total number of messages routed to DLQ.\n"+
			"# TYPE simplemq_dead_lettered_total counter\n"+
			"simplemq_dead_lettered_total %d\n"+
			"# HELP simplemq_expired_total Total number of messages expired by TTL.\n"+
			"# TYPE simplemq_expired_total counter\n"+
			"simplemq_expired_total %d\n"+
			"# HELP simplemq_dropped_total Total number of dropped fan-out deliveries.\n"+
			"# TYPE simplemq_dropped_total counter\n"+
			"simplemq_dropped_total %d\n"+
			"# HELP simplemq_queues Current number of non-empty queues.\n"+
			"# TYPE simplemq_queues gauge\n"+
			"simplemq_queues %d\n"+
			"# HELP simplemq_queued_messages Current number of messages waiting in queues.\n"+
			"# TYPE simplemq_queued_messages gauge\n"+
			"simplemq_queued_messages %d\n"+
			"# HELP simplemq_delayed_messages Current number of delayed messages.\n"+
			"# TYPE simplemq_delayed_messages gauge\n"+
			"simplemq_delayed_messages %d\n"+
			"# HELP simplemq_inflight_messages Current number of unacknowledged in-flight messages.\n"+
			"# TYPE simplemq_inflight_messages gauge\n"+
			"simplemq_inflight_messages %d\n"+
			"# HELP simplemq_active_subscribers Current number of active subscribers.\n"+
			"# TYPE simplemq_active_subscribers gauge\n"+
			"simplemq_active_subscribers %d\n",
		m.PublishedTotal,
		m.DeliveredTotal,
		m.AckedTotal,
		m.NackedTotal,
		m.RedeliveredTotal,
		m.DeadLetteredTotal,
		m.ExpiredTotal,
		m.DroppedTotal,
		m.QueueCount,
		m.TotalQueued,
		m.TotalDelayed,
		m.TotalInFlight,
		m.ActiveSubscribers,
	)
}

func (h *handlerSet) parseConsumeQuery(req *http.Request) (broker.ConsumeOptions, int, error) {
	q := req.URL.Query()
	opts := broker.ConsumeOptions{
		Timeout: h.opts.DefaultPollTimeout,
	}

	if raw := q.Get("timeout"); raw != "" {
		if raw == "0" {
			opts.Timeout = 0
		} else {
			d, err := time.ParseDuration(raw)
			if err != nil || d < 0 {
				return opts, 0, fmt.Errorf("invalid timeout %q", raw)
			}
			opts.Timeout = d
		}
	}

	if raw := q.Get("ack"); raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return opts, 0, fmt.Errorf("invalid ack flag %q", raw)
		}
		opts.AckRequired = b
	}

	if raw := q.Get("visibility"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return opts, 0, fmt.Errorf("invalid visibility timeout %q", raw)
		}
		opts.VisibilityTimeout = d
	}

	if raw := q.Get("mode"); raw != "" {
		m, err := broker.ParseDeliveryMode(raw, "")
		if err != nil {
			return opts, 0, err
		}
		opts.Mode = m
	}

	maxBatch := 1
	if raw := q.Get("max"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > 1000 {
			return opts, 0, fmt.Errorf("invalid max %q: must be 1-1000", raw)
		}
		maxBatch = n
	}

	return opts, maxBatch, nil
}

func (h *handlerSet) decodeStrictJSON(rw http.ResponseWriter, req *http.Request, dst any) error {
	req.Body = http.MaxBytesReader(rw, req.Body, h.opts.MaxBodyBytes)
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	// Reject trailing garbage after the first JSON value.
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain a single JSON value")
	}
	return nil
}

func (h *handlerSet) handleDecodeError(rw http.ResponseWriter, req *http.Request, err error) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		writeJSONError(rw, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	h.opts.Logger.Warn("invalid JSON payload", "remote", req.RemoteAddr, "err", err)
	writeJSONError(rw, http.StatusBadRequest, "invalid JSON payload: "+err.Error())
}

func (h *handlerSet) handleBrokerError(rw http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, model.ErrInvalidQueueName),
		errors.Is(err, model.ErrMissingType),
		errors.Is(err, model.ErrInvalidTTL),
		errors.Is(err, model.ErrInvalidDelay):
		writeJSONError(rw, http.StatusBadRequest, err.Error())
	case errors.Is(err, broker.ErrReceiptNotFound):
		writeJSONError(rw, http.StatusNotFound, err.Error())
	case errors.Is(err, broker.ErrQueueFull), errors.Is(err, broker.ErrMaxQueuesReached):
		writeJSONError(rw, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, broker.ErrBrokerClosed):
		writeJSONError(rw, http.StatusServiceUnavailable, err.Error())
	default:
		writeJSONError(rw, http.StatusInternalServerError, err.Error())
	}
}

func (h *handlerSet) handleConsumeError(rw http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, broker.ErrTimeout):
		rw.WriteHeader(http.StatusNoContent)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// Client disconnected; no response needed.
	default:
		h.handleBrokerError(rw, err)
	}
}

func writeJSON(rw http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(rw, `{"error":"internal marshal error"}`)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_, _ = rw.Write(data)
}

func writeJSONError(rw http.ResponseWriter, status int, msg string) {
	writeJSON(rw, status, map[string]string{"error": msg})
}

func splitNonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	raw := strings.Split(s, "/")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
