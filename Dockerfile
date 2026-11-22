# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/simple-mq .

FROM scratch
COPY --from=builder /out/simple-mq /simple-mq
EXPOSE 8001 8002 8003
VOLUME ["/data"]
ENV SMQ_DATA_FILE=/data/queues.json
ENTRYPOINT ["/simple-mq"]
