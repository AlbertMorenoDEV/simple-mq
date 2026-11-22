# Simple MQ

Simple MQ is a lightweight, **zero-dependency** message queue written in Go. It provides HTTP (REST + Long Polling + SSE streaming) and NDJSON TCP interfaces with crash-safe WAL persistence, at-least-once delivery (`ACK`/`NACK` + Dead-Letter Queues), message TTL & delayed delivery, configurable delivery modes, Prometheus metrics, and an embedded Go client SDK.

## Features

- **Zero External Dependencies**: Built 100% on the Go standard library (`net/http`, `net`, `log/slog`, `crypto/rand`).
- **Dual-Port HTTP + SSE API**: Publisher/Admin port (`:8001`) and Subscriber/SSE port (`:8002`) with strict JSON decoding, body size limits, and optional Bearer token authentication.
- **Persistent & One-Shot TCP Socket API**: NDJSON TCP server (`:8003`) supporting publish, batch publish, one-shot or streaming subscribe, multiplexed `ACK`/`NACK`, `ping`, `stats`, and instant disconnect cleanup.
- **Multiple Delivery Modes**: `hybrid` (default: live fan-out or FIFO queue), `queue` (competing consumers / round-robin), and `fanout` (broadcast).
- **At-Least-Once Delivery & DLQ**: Optional `ACK`/`NACK` receipts with configurable visibility timeouts, automatic redelivery, and Dead-Letter Queue (`<queue>.dlq`) routing after max retries.
- **TTL & Delayed Messages**: Per-message expiration (`ttl`) and scheduled future delivery (`delay`).
- **Crash-Safe Persistence**: Append-only Write-Ahead Log (`wal`, default) with periodic atomic snapshot compaction (`queues.json` + `queues.json.wal`), atomic JSON snapshot mode (`json`), or ephemeral in-memory mode (`memory`). Backwards-compatible with legacy v0.2 `queues.json`.
- **Observability & Admin API**: `/healthz`, `/readyz`, Prometheus `/metrics`, queue listing, stats, non-destructive `peek`, and `DELETE` purge.
- **Graceful Shutdown**: Handles `SIGINT`/`SIGTERM`, drains active HTTP/TCP servers, and flushes a compacted snapshot before exit.
- **Go Client SDK**: Ready-to-use HTTP and TCP clients in [`pkg/client`](pkg/client/client.go).

## Architecture

```text
.
├── main.go                       # CLI entrypoint, slog setup, graceful shutdown
├── internal/
│   ├── config/                   # CLI flags + SMQ_* env var configuration & validation
│   ├── model/                    # Message schema, UUIDv4 generator, TTL/delay & queue validation
│   ├── broker/                   # Core queue engine, leak-free RingBuffer, ACK/DLQ/TTL/delay reaper
│   ├── storage/                  # Store interface: WALStore (default), JSONStore (atomic), MemoryStore
│   └── transport/
│       ├── http/                 # Publisher/Admin (:8001), Subscriber/SSE (:8002), Prometheus /metrics
│       └── tcp/                  # NDJSON TCP server (:8003) with streaming & disconnect detection
├── pkg/client/                   # Zero-dependency Go HTTP & TCP client SDK
├── Dockerfile                    # Multi-stage scratch container image
└── Makefile                      # Common build/vet/test/coverage/docker targets
```

## Prerequisites

- [Go](https://golang.org/dl/) 1.27+

## Installation & Running

1. **Clone the repository:**
   ```bash
   git clone https://github.com/AlbertMorenoDEV/simple-mq.git
   cd simple-mq
   ```

2. **Run the server:**
   ```bash
   go run .
   # or: make run
   ```

3. **Or run via Docker:**
   ```bash
   docker build -t simple-mq:latest .
   docker run -p 8001:8001 -p 8002:8002 -p 8003:8003 -v simplemq-data:/data simple-mq:latest
   ```

Default listening addresses:
- **Publisher & Admin (HTTP)**: `:8001`
- **Subscriber & SSE (HTTP)**: `:8002`
- **Socket (TCP NDJSON)**: `:8003`

## Configuration

Every setting can be configured via environment variable or CLI flag (flags override env vars):

| CLI Flag | Environment Variable | Default | Description |
| :--- | :--- | :--- | :--- |
| `-pub-addr` | `SMQ_PUB_ADDR` | `:8001` | Publisher & Admin HTTP listen address |
| `-sub-addr` | `SMQ_SUB_ADDR` | `:8002` | Subscriber & SSE HTTP listen address |
| `-tcp-addr` | `SMQ_TCP_ADDR` | `:8003` | TCP NDJSON listen address |
| `-data-file` | `SMQ_DATA_FILE` | `queues.json` | Persistence snapshot file path (`<file>.wal` for WAL) |
| `-store` | `SMQ_STORE` | `wal` | Persistence backend: `wal`, `json`, or `memory` |
| `-delivery-mode` | `SMQ_DELIVERY_MODE` | `hybrid` | Default delivery pattern: `hybrid`, `queue`, or `fanout` |
| `-max-message-bytes` | `SMQ_MAX_MESSAGE_BYTES` | `1048576` | Max HTTP body / TCP line payload size (1 MiB) |
| `-max-queue-depth` | `SMQ_MAX_QUEUE_DEPTH` | `100000` | Max messages buffered per queue |
| `-max-queues` | `SMQ_MAX_QUEUES` | `10000` | Max distinct active queues |
| `-poll-timeout` | `SMQ_POLL_TIMEOUT` | `30s` | Default long-poll timeout |
| `-tcp-idle-timeout` | `SMQ_TCP_IDLE_TIMEOUT` | `5m` | TCP idle read timeout |
| `-visibility-timeout` | `SMQ_VISIBILITY_TIMEOUT` | `30s` | Default lease duration for `ack=true` consumers |
| `-max-retries` | `SMQ_MAX_RETRIES` | `3` | Delivery attempts before routing to `<queue>.dlq` |
| `-wal-compact-every` | `SMQ_WAL_COMPACT_EVERY` | `1000` | WAL entries between automatic snapshot compactions |
| `-auth-token` | `SMQ_AUTH_TOKEN` | *(empty)* | Optional shared bearer token (HTTP `Authorization: Bearer ...`, TCP `auth` action) |
| `-log-level` | `SMQ_LOG_LEVEL` | `info` | Structured log level: `debug`, `info`, `warn`, `error` |
| `-log-format` | `SMQ_LOG_FORMAT` | `text` | Structured log format: `text` or `json` |

## Usage

### Message Format

```json
{
  "id": "optional-unique-id",
  "type": "order.created",
  "data": { "order_id": 42, "total": 19.99 },
  "headers": { "source": "checkout" },
  "ttl": "5m",
  "delay": "10s"
}
```

- `type` is required.
- `id` is optional; if omitted, an RFC 4122 UUIDv4 is generated automatically.
- `data` accepts **any valid JSON** (strings, objects, arrays, numbers, booleans).
- `ttl` and `delay` accept Go duration strings (e.g., `500ms`, `30s`, `5m`).
- Queue names must match `^[a-zA-Z0-9._-]{1,128}$` (empty path defaults to `default`).

### HTTP API

#### Publish a Message (`POST :8001/{queue}`)

```bash
curl -X POST http://localhost:8001/my-queue \
  -H "Content-Type: application/json" \
  -d '{"id":"1","type":"test","data":"hello"}'
# 201 Created -> {"id":"1","queue":"my-queue","status":"published"}
```

#### Batch Publish (`POST :8001/{queue}/batch`)

```bash
curl -X POST http://localhost:8001/my-queue/batch \
  -H "Content-Type: application/json" \
  -d '[{"type":"job","data":{"n":1}},{"type":"job","data":{"n":2}}]'
# 201 Created -> {"count":2,"ids":[...],"queue":"my-queue","status":"published"}
```

#### Consume Messages (`GET :8002/{queue}`)

```bash
# Long-poll (default 30s timeout; returns 204 No Content on timeout)
curl http://localhost:8002/my-queue

# Non-blocking poll
curl "http://localhost:8002/my-queue?timeout=0"

# Batch consume up to 10 messages
curl "http://localhost:8002/my-queue?max=10&timeout=5s"

# Competing-consumer mode with at-least-once ACK
curl -i "http://localhost:8002/my-queue?mode=queue&ack=true&visibility=30s"
# Response includes receipt in JSON body and X-Message-Receipt header
```

#### Acknowledge / Reject (`POST :8002/{queue}/ack/{receipt}` or `nack`)

```bash
curl -X POST http://localhost:8002/my-queue/ack/<receipt>
curl -X POST http://localhost:8002/my-queue/nack/<receipt>
```

#### Server-Sent Events Stream (`GET :8002/{queue}/stream`)

```bash
curl -N http://localhost:8002/my-queue/stream
```

#### Admin, Health & Prometheus Metrics (`:8001`)

| Endpoint | Description |
| :--- | :--- |
| `GET /healthz`, `GET /readyz` | Liveness/readiness check (always accessible without auth) |
| `GET /metrics` | Prometheus text exposition metrics (`simplemq_*`) |
| `GET /queues` | List all active queues with depth, delayed, in-flight, and subscriber counts |
| `GET /queues/{name}/stats` | Detailed stats for a single queue |
| `GET /queues/{name}/peek?limit=10` | Inspect up to `limit` queued messages without consuming |
| `DELETE /queues/{name}` | Purge all queued, delayed, and in-flight messages in `{name}` |

### TCP Socket API (`:8003`)

The TCP server speaks newline-delimited JSON (NDJSON):

```json
{"action": "ping"}
{"action": "auth", "token": "my-secret"}
{"action": "publish", "queue": "tasks", "message": {"type": "test", "data": "socket-msg"}}
{"action": "publish_batch", "queue": "tasks", "messages": [{"type": "t1"}, {"type": "t2"}]}
{"action": "subscribe", "queue": "tasks", "timeout": "10s", "ack": true}
{"action": "subscribe", "queue": "tasks", "stream": true, "ack": true}
{"action": "ack", "queue": "tasks", "receipt": "<receipt>"}
{"action": "nack", "queue": "tasks", "receipt": "<receipt>"}
{"action": "unsubscribe"}
{"action": "stats", "queue": "tasks"}
```

- Setting `"stream": true` keeps the subscription open for continuous delivery while allowing multiplexed `ack`, `nack`, `ping`, `publish`, and `unsubscribe` commands on the same connection.
- Disconnecting immediately cleans up subscriber registrations without leaking channels or dropping future messages.

### Go Client SDK

```go
hc := client.NewHTTPClient("http://localhost:8001", "http://localhost:8002", "")
id, err := hc.Publish(ctx, "tasks", client.Message{
    Type: "email.send",
    Data: json.RawMessage(`{"to":"dev@example.com"}`),
})

msg, err := hc.Consume(ctx, "tasks", client.ConsumeOptions{Ack: true})
_ = hc.Ack(ctx, "tasks", msg.Receipt)
```

## Testing

The test suite is fully self-contained (in-process HTTP/TCP servers with isolated temp directories) and runs with the Go race detector:

```bash
go test -race -cover -v ./...
# or: make test
```

---

## Audit & Improvement Tracker

Tracking all findings, bug fixes, hardening tasks, and feature proposals from the repository audit:

### Phase 1 — Critical Bug Fixes & Reliability
- [x] **Self-contained CI & tests**: Tests run in-process with `httptest` / ephemeral TCP ports and `t.TempDir()`, verified with `go test -race -cover ./...` and `go vet ./...` in CI.
- [x] **TCP subscriber timeout & disconnect leak**: Cleaned up subscriber channels immediately on timeout or socket EOF so subsequent publishes are never dropped into abandoned channels.
- [x] **Persistence race conditions & crash safety**: Serialized persistence writes with strict lock ordering (`persistMu` -> `mu`), atomic temp-file + `fsync` + `os.Rename`, skipped redundant disk writes on pure live fan-out, and preserved legacy v0.2 `queues.json` compatibility.
- [x] **TCP JSON injection**: Replaced `fmt.Fprintf` string interpolation with safe `json.NewEncoder` serialization across all TCP responses.
- [x] **HTTP disconnect delivery race**: Protected canceled one-shot & streaming subscribers by draining and re-queueing any sole-recipient message that raced with cancellation.
- [x] **Queue slice backing-array memory leak**: Replaced `q[1:]` re-slicing with a generic [`RingBuffer`](internal/broker/ring.go) that zeroes popped slots and shrinks/releases idle capacity, plus map key cleanup on empty queues.
- [x] **TCP >64 KB payload truncation & silent errors**: Raised `bufio.Scanner` max buffer to configurable `MaxMessageBytes` (default 1 MiB) and explicitly reported `bufio.ErrTooLong` and write/marshal errors.

### Phase 2 — Security, Hardening & Architecture
- [x] **Modular package architecture**: Refactored monolithic `main.go` into `internal/config`, `internal/model`, `internal/broker`, `internal/storage`, `internal/transport/http`, `internal/transport/tcp`, and `pkg/client` (eliminating global mutable state and `init()` side effects).
- [x] **Zero third-party dependencies**: Removed unmaintained `baloo.v3`, `icrowley/fake`, and `google/uuid` in favor of Go stdlib (`net/http/httptest`, `crypto/rand`).
- [x] **DoS protection & input validation**: Added `http.MaxBytesReader` (`413`), strict JSON decoding (`DisallowUnknownFields` + single-value enforcement), queue name validation (`^[a-zA-Z0-9._-]{1,128}$`), and configurable `MaxQueueDepth` / `MaxQueues` limits (`429`).
- [x] **HTTP & TCP timeouts**: Added `ReadHeaderTimeout`, `IdleTimeout`, HTTP `?timeout=` (`204 No Content`), and TCP idle read deadlines.
- [x] **Graceful shutdown**: Added `SIGINT`/`SIGTERM` handling with ordered HTTP/TCP draining and final compacted snapshot flush.
- [x] **Configuration & Authentication**: Added CLI flags, `SMQ_*` env vars, and optional constant-time Bearer/TCP token authentication.
- [x] **Structured logging**: Migrated from manual `log.Printf` prefixes to stdlib `log/slog` with configurable level and text/JSON formats.
- [x] **Packaging & CI**: Added `go vet` + `-race` to [`.github/workflows/go.yml`](.github/workflows/go.yml), multi-stage `Dockerfile`, and `Makefile`.

### Phase 3 — New Features Implemented
- [x] **At-Least-Once Delivery (`ACK`/`NACK` + Visibility Timeout + DLQ)**: Receipt leases, automatic expiration reaper, redelivery counter, crash recovery for unacked messages, and `<queue>.dlq` routing.
- [x] **Explicit Delivery Modes**: Competing consumers (`queue` / round-robin), broadcast (`fanout`), and backwards-compatible `hybrid`.
- [x] **Message TTL & Delayed Delivery**: Per-message `ttl` expiration and `delay` scheduled delivery.
- [x] **Arbitrary JSON Payloads & Auto-IDs**: `json.RawMessage` payloads, optional headers/timestamps, and automatic UUIDv4 generation.
- [x] **Write-Ahead Log (WAL) + Compaction**: O(1) append-only NDJSON WAL with crash-tolerant replay and automatic atomic compaction.
- [x] **Batch Operations**: `POST /{queue}/batch`, `GET /{queue}?max=N`, and TCP `publish_batch`.
- [x] **Persistent Streaming**: HTTP SSE (`GET /{queue}/stream`) with keepalives and multiplexed TCP streaming (`"stream": true` + `unsubscribe`).
- [x] **Admin API & Prometheus Metrics**: `/healthz`, `/readyz`, `/metrics`, `/queues`, `/queues/{name}/stats`, `/queues/{name}/peek`, and `DELETE /queues/{name}`.
- [x] **Go Client SDK**: [`pkg/client`](pkg/client/client.go) supporting both HTTP and TCP transports.

### Future Ideas (Backlog)
- [ ] **TLS / mTLS Support**: Native HTTPS and TLS-wrapped TCP listener options.
- [ ] **Embedded Web Dashboard**: Lightweight read-only HTML UI backed by `/queues` and `/metrics`.
- [ ] **Consumer Groups & Offsets**: Durable named consumer groups with independent cursors per topic.
- [ ] **WebSocket Transport**: Browser-friendly bidirectional pub/sub transport.

---

## Changelog

### v0.3.0
- Modularized codebase into `internal/*` and `pkg/client` with zero external dependencies.
- Fixed TCP subscriber timeout/disconnect channel leaks, JSON injection, persistence races/atomicity, HTTP cancel drop race, and slice backing-array memory leaks.
- Added crash-safe WAL persistence with automatic compaction and legacy v0.2 snapshot compatibility.
- Added at-least-once delivery (`ACK`/`NACK`, visibility timeout, DLQ), competing consumers (`mode=queue`), TTL & delayed delivery, batch publish/consume, SSE & TCP streaming, Admin API, Prometheus `/metrics`, optional auth, `slog`, graceful shutdown, `Dockerfile`, and `Makefile`.
- Hardened CI with `go vet` and `go test -race -cover`.

### v0.2
- Added TCP Socket support.
- Implemented Fan-out / Long Polling.
- Added Data Persistence.
- Support for Multiple Named Queues.
- Made test suite self-contained with `httptest` servers.

### v0.1
- HTTP Publisher with single message support.
- HTTP Subscriber for message consumption.
- In-memory queue implementation.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
