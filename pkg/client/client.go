package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrNoMessage is returned when a non-blocking or timed-out consume finds no message.
var ErrNoMessage = errors.New("no message available")

// Message mirrors simple-mq's wire JSON message schema.
type Message struct {
	ID            string            `json:"id,omitempty"`
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

// ConsumeOptions configures HTTP or TCP consume requests.
type ConsumeOptions struct {
	Timeout    *time.Duration
	Ack        bool
	Visibility time.Duration
	Mode       string
}

// HTTPClient interacts with simple-mq's publisher (:8001) and subscriber (:8002) HTTP APIs.
type HTTPClient struct {
	pubURL     string
	subURL     string
	authToken  string
	httpClient *http.Client
}

// NewHTTPClient creates an HTTPClient targeting pubURL and subURL.
func NewHTTPClient(pubURL, subURL string, authToken string) *HTTPClient {
	return &HTTPClient{
		pubURL:     strings.TrimRight(pubURL, "/"),
		subURL:     strings.TrimRight(subURL, "/"),
		authToken:  authToken,
		httpClient: &http.Client{},
	}
}

func (c *HTTPClient) do(req *http.Request) (*http.Response, error) {
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	return c.httpClient.Do(req)
}

// Publish sends a single message to queue and returns the assigned message ID.
func (c *HTTPClient) Publish(ctx context.Context, queue string, msg Message) (string, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return "", err
	}
	endpoint := c.pubURL + "/" + url.PathEscape(queue)
	if queue == "" {
		endpoint = c.pubURL + "/"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return "", decodeAPIError(resp)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// PublishBatch publishes multiple messages to queue and returns their assigned IDs.
func (c *HTTPClient) PublishBatch(ctx context.Context, queue string, msgs []Message) ([]string, error) {
	if queue == "" {
		queue = "default"
	}
	body, err := json.Marshal(msgs)
	if err != nil {
		return nil, err
	}
	endpoint := c.pubURL + "/" + url.PathEscape(queue) + "/batch"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return nil, decodeAPIError(resp)
	}
	var out struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.IDs, nil
}

// Consume fetches a single message from queue. Returns ErrNoMessage on HTTP 204.
func (c *HTTPClient) Consume(ctx context.Context, queue string, opts ConsumeOptions) (*Message, error) {
	endpoint := c.subURL + "/" + url.PathEscape(queue)
	if queue == "" {
		endpoint = c.subURL + "/"
	}
	params := buildConsumeQuery(opts, 0)
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil, ErrNoMessage
	}
	if resp.StatusCode != http.StatusOK {
		return nil, decodeAPIError(resp)
	}
	var msg Message
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// ConsumeBatch fetches up to max messages from queue.
func (c *HTTPClient) ConsumeBatch(ctx context.Context, queue string, max int, opts ConsumeOptions) ([]Message, error) {
	if max <= 1 {
		m, err := c.Consume(ctx, queue, opts)
		if err != nil {
			return nil, err
		}
		return []Message{*m}, nil
	}
	endpoint := c.subURL + "/" + url.PathEscape(queue)
	if queue == "" {
		endpoint = c.subURL + "/"
	}
	params := buildConsumeQuery(opts, max)
	endpoint += "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil, ErrNoMessage
	}
	if resp.StatusCode != http.StatusOK {
		return nil, decodeAPIError(resp)
	}
	var msgs []Message
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

// Ack acknowledges a message receipt on queue.
func (c *HTTPClient) Ack(ctx context.Context, queue, receipt string) error {
	return c.ackOrNack(ctx, queue, "ack", receipt)
}

// Nack negatively acknowledges a message receipt on queue.
func (c *HTTPClient) Nack(ctx context.Context, queue, receipt string) error {
	return c.ackOrNack(ctx, queue, "nack", receipt)
}

func (c *HTTPClient) ackOrNack(ctx context.Context, queue, action, receipt string) error {
	if queue == "" {
		queue = "default"
	}
	endpoint := fmt.Sprintf("%s/%s/%s/%s", c.subURL, url.PathEscape(queue), action, url.PathEscape(receipt))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decodeAPIError(resp)
	}
	return nil
}

func buildConsumeQuery(opts ConsumeOptions, max int) url.Values {
	q := url.Values{}
	if opts.Timeout != nil {
		if *opts.Timeout == 0 {
			q.Set("timeout", "0")
		} else {
			q.Set("timeout", opts.Timeout.String())
		}
	}
	if opts.Ack {
		q.Set("ack", "true")
	}
	if opts.Visibility > 0 {
		q.Set("visibility", opts.Visibility.String())
	}
	if opts.Mode != "" {
		q.Set("mode", opts.Mode)
	}
	if max > 1 {
		q.Set("max", strconv.Itoa(max))
	}
	return q
}

func decodeAPIError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var parsed struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err == nil && parsed.Error != "" {
		return fmt.Errorf("http %d: %s", resp.StatusCode, parsed.Error)
	}
	return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
}

// TCPClient is a lightweight NDJSON client for simple-mq's TCP interface (:8003).
type TCPClient struct {
	conn    net.Conn
	enc     *json.Encoder
	scanner *bufio.Scanner
}

// DialTCP connects to addr and optionally authenticates if authToken is non-empty.
func DialTCP(addr string, authToken string) (*TCPClient, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	c := &TCPClient{
		conn:    conn,
		enc:     json.NewEncoder(conn),
		scanner: scanner,
	}
	if authToken != "" {
		if err := c.send(map[string]any{"action": "auth", "token": authToken}); err != nil {
			_ = conn.Close()
			return nil, err
		}
		var resp map[string]string
		if err := c.readJSON(&resp); err != nil {
			_ = conn.Close()
			return nil, err
		}
		if resp["status"] != "authenticated" {
			_ = conn.Close()
			return nil, fmt.Errorf("tcp auth failed: %s", resp["error"])
		}
	}
	return c, nil
}

// Close closes the underlying TCP connection.
func (c *TCPClient) Close() error {
	return c.conn.Close()
}

// Ping verifies connectivity to the TCP server.
func (c *TCPClient) Ping() error {
	if err := c.send(map[string]any{"action": "ping"}); err != nil {
		return err
	}
	var resp map[string]string
	if err := c.readJSON(&resp); err != nil {
		return err
	}
	if resp["status"] != "pong" {
		return fmt.Errorf("unexpected ping response: %v", resp)
	}
	return nil
}

// Publish sends a single message over TCP and returns its ID.
func (c *TCPClient) Publish(queue string, msg Message) (string, error) {
	if err := c.send(map[string]any{
		"action":  "publish",
		"queue":   queue,
		"message": msg,
	}); err != nil {
		return "", err
	}
	var resp struct {
		Status string `json:"status"`
		ID     string `json:"id"`
		Error  string `json:"error"`
	}
	if err := c.readJSON(&resp); err != nil {
		return "", err
	}
	if resp.Error != "" {
		return "", errors.New(resp.Error)
	}
	return resp.ID, nil
}

// SubscribeOne performs a one-shot subscribe on queue.
func (c *TCPClient) SubscribeOne(queue string, opts ConsumeOptions) (*Message, error) {
	req := map[string]any{
		"action": "subscribe",
		"queue":  queue,
		"ack":    opts.Ack,
	}
	if opts.Timeout != nil {
		if *opts.Timeout == 0 {
			req["timeout"] = "0"
		} else {
			req["timeout"] = opts.Timeout.String()
		}
	}
	if opts.Visibility > 0 {
		req["visibility"] = opts.Visibility.String()
	}
	if opts.Mode != "" {
		req["mode"] = opts.Mode
	}
	if err := c.send(req); err != nil {
		return nil, err
	}
	raw, err := c.readLine()
	if err != nil {
		return nil, err
	}
	var statusResp struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(raw, &statusResp); err == nil {
		if statusResp.Error != "" {
			return nil, errors.New(statusResp.Error)
		}
		if statusResp.Status == "timeout" {
			return nil, ErrNoMessage
		}
	}
	var msg Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// Ack sends an ACK over TCP.
func (c *TCPClient) Ack(queue, receipt string) error {
	return c.ackOrNack("ack", queue, receipt)
}

// Nack sends a NACK over TCP.
func (c *TCPClient) Nack(queue, receipt string) error {
	return c.ackOrNack("nack", queue, receipt)
}

func (c *TCPClient) ackOrNack(action, queue, receipt string) error {
	if err := c.send(map[string]any{
		"action":  action,
		"queue":   queue,
		"receipt": receipt,
	}); err != nil {
		return err
	}
	var resp struct {
		Error string `json:"error"`
	}
	if err := c.readJSON(&resp); err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	return nil
}

func (c *TCPClient) send(v any) error {
	return c.enc.Encode(v)
}

func (c *TCPClient) readLine() ([]byte, error) {
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	cp := make([]byte, len(c.scanner.Bytes()))
	copy(cp, c.scanner.Bytes())
	return cp, nil
}

func (c *TCPClient) readJSON(dst any) error {
	raw, err := c.readLine()
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dst)
}
