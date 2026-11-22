package tcptransport

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/AlbertMorenoDEV/simple-mq/internal/broker"
	"github.com/AlbertMorenoDEV/simple-mq/internal/model"
)

// Options configures the TCP NDJSON transport server.
type Options struct {
	MaxMessageBytes    int64
	DefaultPollTimeout time.Duration
	IdleTimeout        time.Duration
	AuthToken          string
	Logger             *slog.Logger
}

func normalizeOptions(opts Options) Options {
	if opts.MaxMessageBytes <= 0 {
		opts.MaxMessageBytes = 1 << 20 // 1 MiB
	}
	if opts.DefaultPollTimeout == 0 {
		opts.DefaultPollTimeout = 30 * time.Second
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 5 * time.Minute
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return opts
}

type socketRequest struct {
	Action     string          `json:"action"`
	Queue      string          `json:"queue,omitempty"`
	Msg        model.Message   `json:"message,omitempty"`
	Messages   []model.Message `json:"messages,omitempty"`
	Receipt    string          `json:"receipt,omitempty"`
	Stream     bool            `json:"stream,omitempty"`
	Ack        bool            `json:"ack,omitempty"`
	Timeout    string          `json:"timeout,omitempty"`
	Visibility string          `json:"visibility,omitempty"`
	Mode       string          `json:"mode,omitempty"`
	Token      string          `json:"token,omitempty"`
}

// Server serves NDJSON messaging commands over TCP with graceful shutdown and
// clean subscriber lifecycle management.
type Server struct {
	broker *broker.Broker
	opts   Options

	mu       sync.Mutex
	ln       net.Listener
	conns    map[net.Conn]struct{}
	closed   bool
	connWg   sync.WaitGroup
	stopCh   chan struct{}
}

// NewServer constructs a TCP Server backed by b.
func NewServer(b *broker.Broker, opts Options) *Server {
	return &Server{
		broker: b,
		opts:   normalizeOptions(opts),
		conns:  make(map[net.Conn]struct{}),
		stopCh: make(chan struct{}),
	}
}

// ListenAndServe binds addr and accepts connections until Shutdown is called.
func (s *Server) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// Serve accepts connections on ln until Shutdown is called.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = ln.Close()
		return net.ErrClosed
	}
	s.ln = ln
	s.mu.Unlock()

	s.opts.Logger.Info("TCP server listening", "addr", ln.Addr().String())

	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			s.opts.Logger.Error("TCP accept failed", "err", err)
			continue
		}

		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return nil
		}
		s.conns[conn] = struct{}{}
		s.connWg.Add(1)
		s.mu.Unlock()

		go func(c net.Conn) {
			defer s.connWg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
				_ = c.Close()
			}()
			s.handleConn(c)
		}(conn)
	}
}

// Shutdown stops accepting new connections, closes active sockets, and waits
// for connection handlers to exit (or ctx to expire).
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.stopCh)
	var lnErr error
	if s.ln != nil {
		lnErr = s.ln.Close()
	}
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.connWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return lnErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

type safeConnWriter struct {
	conn net.Conn
	mu   sync.Mutex
	enc  *json.Encoder
}

func newSafeConnWriter(conn net.Conn) *safeConnWriter {
	return &safeConnWriter{
		conn: conn,
		enc:  json.NewEncoder(conn),
	}
}

func (w *safeConnWriter) writeJSON(v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return w.enc.Encode(v)
}

func (s *Server) handleConn(conn net.Conn) {
	s.opts.Logger.Info("TCP client connected", "remote", conn.RemoteAddr().String())
	writer := newSafeConnWriter(conn)

	connCtx, cancelConn := context.WithCancel(context.Background())
	defer cancelConn()

	// Dedicated reader goroutine detects client disconnects immediately even
	// while a one-shot subscribe is blocking, preventing leaked channels.
	linesCh := make(chan []byte, 8)
	var scanErr error
	go func() {
		defer close(linesCh)
		defer cancelConn()

		scanner := bufio.NewScanner(conn)
		maxBytes := int(s.opts.MaxMessageBytes)
		if maxBytes < 64*1024 {
			maxBytes = 64 * 1024
		}
		scanner.Buffer(make([]byte, 0, 64*1024), maxBytes)

		for {
			_ = conn.SetReadDeadline(time.Now().Add(s.opts.IdleTimeout))
			if !scanner.Scan() {
				scanErr = scanner.Err()
				return
			}
			line := make([]byte, len(scanner.Bytes()))
			copy(line, scanner.Bytes())
			select {
			case linesCh <- line:
			case <-s.stopCh:
				return
			}
		}
	}()

	authenticated := s.opts.AuthToken == ""

	var (
		streamCancel context.CancelFunc
		streamDone   chan struct{}
	)
	stopActiveStream := func() {
		if streamCancel != nil {
			streamCancel()
			<-streamDone
			streamCancel = nil
			streamDone = nil
		}
	}
	defer stopActiveStream()

	for {
		select {
		case <-s.stopCh:
			return
		case line, ok := <-linesCh:
			if !ok {
				if scanErr != nil {
					if errors.Is(scanErr, bufio.ErrTooLong) {
						_ = writer.writeJSON(map[string]string{"error": "payload too large"})
					}
					s.opts.Logger.Debug("TCP read closed", "remote", conn.RemoteAddr().String(), "err", scanErr)
				}
				return
			}

			var req socketRequest
			if err := json.Unmarshal(line, &req); err != nil {
				s.opts.Logger.Warn("invalid TCP JSON", "remote", conn.RemoteAddr().String(), "err", err)
				if err := writer.writeJSON(map[string]string{"error": "invalid JSON"}); err != nil {
					return
				}
				continue
			}

			if req.Action == "ping" {
				if err := writer.writeJSON(map[string]string{"status": "pong"}); err != nil {
					return
				}
				continue
			}

			if req.Action == "auth" {
				if s.opts.AuthToken == "" || subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.opts.AuthToken)) == 1 {
					authenticated = true
					if err := writer.writeJSON(map[string]string{"status": "authenticated"}); err != nil {
						return
					}
				} else {
					if err := writer.writeJSON(map[string]string{"error": "unauthorized"}); err != nil {
						return
					}
				}
				continue
			}

			if !authenticated {
				if err := writer.writeJSON(map[string]string{"error": "unauthorized"}); err != nil {
					return
				}
				continue
			}

			queueName := req.Queue
			if queueName == "" {
				queueName = model.DefaultQueueName
			}

			switch req.Action {
			case "publish":
				pub, err := s.broker.Publish(queueName, req.Msg)
				if err != nil {
					if err := writer.writeJSON(map[string]string{"error": err.Error()}); err != nil {
						return
					}
					continue
				}
				qNorm, _ := model.ValidateQueueName(queueName)
				if err := writer.writeJSON(map[string]string{
					"status": "published",
					"id":     pub.ID,
					"queue":  qNorm,
				}); err != nil {
					return
				}

			case "publish_batch":
				pubs, err := s.broker.PublishBatch(queueName, req.Messages)
				if err != nil {
					if err := writer.writeJSON(map[string]string{"error": err.Error()}); err != nil {
						return
					}
					continue
				}
				ids := make([]string, len(pubs))
				for i, m := range pubs {
					ids[i] = m.ID
				}
				qNorm, _ := model.ValidateQueueName(queueName)
				if err := writer.writeJSON(map[string]any{
					"status": "published",
					"queue":  qNorm,
					"count":  len(pubs),
					"ids":    ids,
				}); err != nil {
					return
				}

			case "subscribe":
				consumeOpts, err := s.parseConsumeOpts(req)
				if err != nil {
					if err := writer.writeJSON(map[string]string{"error": err.Error()}); err != nil {
						return
					}
					continue
				}

				if req.Stream {
					stopActiveStream()
					sCtx, sCancel := context.WithCancel(connCtx)
					ch, unsub, err := s.broker.SubscribeStream(sCtx, queueName, consumeOpts)
					if err != nil {
						sCancel()
						if err := writer.writeJSON(map[string]string{"error": err.Error()}); err != nil {
							return
						}
						continue
					}
					qNorm, _ := model.ValidateQueueName(queueName)
					if err := writer.writeJSON(map[string]string{
						"status": "subscribed",
						"queue":  qNorm,
					}); err != nil {
						sCancel()
						unsub()
						return
					}

					done := make(chan struct{})
					streamCancel = sCancel
					streamDone = done
					go func() {
						defer close(done)
						defer unsub()
						for msg := range ch {
							if err := writer.writeJSON(msg); err != nil {
								cancelConn()
								return
							}
						}
					}()
					continue
				}

				// One-shot subscribe (backwards-compatible) with immediate disconnect detection.
				msg, err := s.broker.Consume(connCtx, queueName, consumeOpts)
				if err != nil {
					if errors.Is(err, broker.ErrTimeout) {
						if err := writer.writeJSON(map[string]string{"status": "timeout"}); err != nil {
							return
						}
						continue
					}
					if errors.Is(err, context.Canceled) || errors.Is(err, broker.ErrBrokerClosed) {
						return
					}
					if err := writer.writeJSON(map[string]string{"error": err.Error()}); err != nil {
						return
					}
					continue
				}
				if err := writer.writeJSON(msg); err != nil {
					return
				}

			case "unsubscribe":
				stopActiveStream()
				if err := writer.writeJSON(map[string]string{"status": "unsubscribed"}); err != nil {
					return
				}

			case "ack":
				if err := s.broker.Ack(queueName, req.Receipt); err != nil {
					if err := writer.writeJSON(map[string]string{"error": err.Error()}); err != nil {
						return
					}
					continue
				}
				if err := writer.writeJSON(map[string]string{"status": "acked", "receipt": req.Receipt}); err != nil {
					return
				}

			case "nack":
				if err := s.broker.Nack(queueName, req.Receipt); err != nil {
					if err := writer.writeJSON(map[string]string{"error": err.Error()}); err != nil {
						return
					}
					continue
				}
				if err := writer.writeJSON(map[string]string{"status": "nacked", "receipt": req.Receipt}); err != nil {
					return
				}

			case "stats":
				if req.Queue == "" {
					if err := writer.writeJSON(map[string]any{"status": "ok", "queues": s.broker.ListQueues()}); err != nil {
						return
					}
					continue
				}
				info, err := s.broker.QueueStats(queueName)
				if err != nil {
					if err := writer.writeJSON(map[string]string{"error": err.Error()}); err != nil {
						return
					}
					continue
				}
				if err := writer.writeJSON(map[string]any{"status": "ok", "stats": info}); err != nil {
					return
				}

			default:
				if err := writer.writeJSON(map[string]string{"error": "unknown action"}); err != nil {
					return
				}
			}
		}
	}
}

func (s *Server) parseConsumeOpts(req socketRequest) (broker.ConsumeOptions, error) {
	opts := broker.ConsumeOptions{
		Timeout:     s.opts.DefaultPollTimeout,
		AckRequired: req.Ack,
	}
	if req.Timeout != "" {
		if req.Timeout == "0" {
			opts.Timeout = 0
		} else {
			d, err := time.ParseDuration(req.Timeout)
			if err != nil || d < 0 {
				return opts, errors.New("invalid timeout duration")
			}
			opts.Timeout = d
		}
	}
	if req.Visibility != "" {
		d, err := time.ParseDuration(req.Visibility)
		if err != nil || d <= 0 {
			return opts, errors.New("invalid visibility duration")
		}
		opts.VisibilityTimeout = d
	}
	if req.Mode != "" {
		m, err := broker.ParseDeliveryMode(req.Mode, "")
		if err != nil {
			return opts, err
		}
		opts.Mode = m
	}
	return opts, nil
}
