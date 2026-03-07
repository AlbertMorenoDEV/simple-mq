# Simple MQ

Simple MQ is a lightweight, in-memory message queue written in Go. It provides a straightforward HTTP interface for publishing and consuming messages via separate ports for publishers and subscribers.

## Features

- **HTTP API**: Simple REST-like interface for all operations.
- **Port Separation**: Distinct ports for publishing (8001) and subscribing (8002) to avoid traffic congestion and simplify security.
- **FIFO Delivery**: Messages are consumed in the same order they were published.
- **JSON Support**: Native handling of JSON message payloads.
- **Minimal Dependencies**: The core server is built using only the Go standard library.

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
- **Publisher Port**: `8001`
- **Subscriber Port**: `8002`

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

### Publish a Message

To add a message to the queue, send a `POST` request to the publisher port:

```bash
curl --header "Content-Type: application/json" \
  --request POST \
  --data '{"id":"1","type":"test","data":"hello"}' \
  http://localhost:8001/
```

### Consume a Message

To retrieve and remove the next message from the queue, send a `GET` request to the subscriber port:

```bash
curl http://localhost:8002/
```

- **Success (200 OK)**: Returns the oldest message in the queue.
- **Queue Empty (204 No Content)**: If there are no messages to deliver.

## Testing

This project uses [baloo](https://github.com/h2non/baloo) for end-to-end testing.

### Running Tests

1. Ensure the server is running in one terminal (`go run main.go`).
2. Run the tests in another terminal:
   ```bash
   go test -v end2end_test.go
   ```

*Note: You may need to install the test dependencies first:*
```bash
go get github.com/icrowley/fake
go get github.com/satori/go.uuid
go get gopkg.in/h2non/baloo.v3
```

## Changelog

### v0.1
- HTTP Publisher with a single message support.
- HTTP Subscriber for message consumption.
- In-memory queue implementation.

## ToDo

- [x] **Persistence**: Save queue items to disk to prevent data loss on restart.
- [ ] **Multiple Subscribers**: Implement different delivery patterns (e.g., fan-out, round-robin).
- [x] **Multiple Queues**: Support for multiple named queues.
- [ ] **Socket Support**: Implement Socket-based publisher and subscriber for lower latency.
- [ ] **Robustness**: Improve error handling, validation, and logging.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
