# Simple MQ

Simple MQ is a lightweight, in-memory message queue written in Go. It provides a straightforward HTTP interface and a high-performance TCP socket interface for publishing and consuming messages.

## Features

- **HTTP API**: Simple REST-like interface for all operations.
- **Socket Support**: TCP server on port 8003 for low-latency messaging.
- **Port Separation**: Distinct ports for publishing (8001) and subscribing (8002).
- **FIFO Delivery**: Messages are consumed in the same order they were published.
- **Fan-out Support**: Active subscribers receive messages immediately (Long Polling / Push).
- **Persistence**: Messages are saved to disk to survive restarts.
- **Multiple Queues**: Support for named queues via URL paths or JSON payloads.

## Prerequisites

- [Go](https://golang.org/dl/) (1.11+ recommended)

## Installation & Running

1. **Clone the repository:**
   ```bash
   git clone https://github.com/AlbertMorenoDEV/simple-mq.git
   cd simple-mq
   ```

2. **Run the server:**
   ```bash
   go run main.go
   ```

The server will start listening on:
- **Publisher Port (HTTP)**: `8001`
- **Subscriber Port (HTTP)**: `8002`
- **Socket Port (TCP)**: `8003`

## Usage

### Message Format

All messages should follow this JSON structure:

```json
{
  "id": "unique-id",
  "type": "message-type",
  "data": "payload-content"
}
```

### HTTP API

#### Publish a Message

```bash
curl --header "Content-Type: application/json" \
  --request POST \
  --data '{"id":"1","type":"test","data":"hello"}' \
  http://localhost:8001/my-queue
```

#### Consume a Message

```bash
curl http://localhost:8002/my-queue
```

### TCP Socket API

The TCP server on port `8003` accepts JSON-per-line.

#### Publish via Socket

Send a JSON object with `action: "publish"`:

```json
{"action": "publish", "queue": "tasks", "message": {"id": "1", "type": "test", "data": "socket-msg"}}
```

#### Subscribe via Socket

Send a JSON object with `action: "subscribe"`:

```json
{"action": "subscribe", "queue": "tasks"}
```

## Testing

This project uses [baloo](https://github.com/h2non/baloo) for end-to-end testing.

### Running Tests

1. Ensure the server is running in one terminal (`go run main.go`).
2. Run the tests in another terminal:
   ```bash
   go test -v .
   ```

## Changelog

### v0.2 (Work in Progress)
- Added TCP Socket support.
- Implemented Fan-out / Long Polling.
- Added Data Persistence.
- Support for Multiple Named Queues.

### v0.1
- HTTP Publisher with a single message support.
- HTTP Subscriber for message consumption.
- In-memory queue implementation.

## ToDo

- [x] **Persistence**: Save queue items to disk to prevent data loss on restart.
- [x] **Multiple Subscribers**: Implement different delivery patterns (e.g., fan-out, round-robin).
- [x] **Multiple Queues**: Support for multiple named queues.
- [x] **Socket Support**: Implement Socket-based publisher and subscriber for lower latency.
- [ ] **Robustness**: Improve error handling, validation, and logging.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
