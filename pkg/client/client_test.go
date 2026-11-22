package client

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AlbertMorenoDEV/simple-mq/internal/broker"
	"github.com/AlbertMorenoDEV/simple-mq/internal/storage"
	httptransport "github.com/AlbertMorenoDEV/simple-mq/internal/transport/http"
	tcptransport "github.com/AlbertMorenoDEV/simple-mq/internal/transport/tcp"
)

func TestHTTPAndTCPClients(t *testing.T) {
	b, err := broker.New(broker.DefaultOptions(), storage.NewMemoryStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	pubSrv := httptest.NewServer(httptransport.NewPublisherHandler(b, httptransport.Options{AuthToken: "tok"}))
	subSrv := httptest.NewServer(httptransport.NewSubscriberHandler(b, httptransport.Options{AuthToken: "tok"}))
	defer pubSrv.Close()
	defer subSrv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcpSrv := tcptransport.NewServer(b, tcptransport.Options{AuthToken: "tok"})
	go func() { _ = tcpSrv.Serve(ln) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tcpSrv.Shutdown(ctx)
	}()

	// HTTP client test.
	hc := NewHTTPClient(pubSrv.URL, subSrv.URL, "tok")
	ctx := context.Background()
	id, err := hc.Publish(ctx, "sdk-http", Message{
		Type: "greeting",
		Data: json.RawMessage(`{"hello":"world"}`),
	})
	if err != nil || id == "" {
		t.Fatalf("HTTP Publish: id=%q err=%v", id, err)
	}

	zero := time.Duration(0)
	got, err := hc.Consume(ctx, "sdk-http", ConsumeOptions{Timeout: &zero, Ack: true})
	if err != nil || got.ID != id || got.Receipt == "" {
		t.Fatalf("HTTP Consume: got=%+v err=%v", got, err)
	}
	if err := hc.Ack(ctx, "sdk-http", got.Receipt); err != nil {
		t.Fatalf("HTTP Ack: %v", err)
	}

	// TCP client test.
	tc, err := DialTCP(ln.Addr().String(), "tok")
	if err != nil {
		t.Fatalf("DialTCP: %v", err)
	}
	defer tc.Close()

	if err := tc.Ping(); err != nil {
		t.Fatalf("TCP Ping: %v", err)
	}
	tid, err := tc.Publish("sdk-tcp", Message{Type: "event", Data: json.RawMessage(`"ok"`)})
	if err != nil || tid == "" {
		t.Fatalf("TCP Publish: id=%q err=%v", tid, err)
	}
	tmsg, err := tc.SubscribeOne("sdk-tcp", ConsumeOptions{Timeout: &zero, Ack: true})
	if err != nil || tmsg.ID != tid || tmsg.Receipt == "" {
		t.Fatalf("TCP SubscribeOne: msg=%+v err=%v", tmsg, err)
	}
	if err := tc.Ack("sdk-tcp", tmsg.Receipt); err != nil {
		t.Fatalf("TCP Ack: %v", err)
	}
}
