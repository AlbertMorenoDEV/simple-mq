.PHONY: build run test vet cover clean docker

BINARY := simple-mq

build:
	go build -v -o $(BINARY) .

run:
	go run .

vet:
	go vet ./...

test:
	go test -race -cover -v ./...

cover:
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

clean:
	rm -f $(BINARY) coverage.out queues.json queues.json.wal

docker:
	docker build -t simple-mq:latest .
