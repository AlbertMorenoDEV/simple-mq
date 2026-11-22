package main

import (
	"path/filepath"
	"testing"

	"github.com/AlbertMorenoDEV/simple-mq/internal/broker"
	"github.com/AlbertMorenoDEV/simple-mq/internal/storage"
	httptransport "github.com/AlbertMorenoDEV/simple-mq/internal/transport/http"
	"net/http/httptest"
)

func TestPersistence(t *testing.T) {
	for _, storeType := range []string{"json", "wal"} {
		t.Run(storeType, func(t *testing.T) {
			dataFile := filepath.Join(t.TempDir(), "queues.json")
			makeStore := func() storage.Store {
				if storeType == "json" {
					return storage.NewJSONStore(dataFile)
				}
				ws, err := storage.NewWALStore(dataFile, 100)
				if err != nil {
					t.Fatalf("NewWALStore: %v", err)
				}
				return ws
			}

			msg := createSampleMessage()

			// 1. Start first broker + HTTP publisher and publish a message.
			b1, err := broker.New(broker.DefaultOptions(), makeStore(), nil)
			if err != nil {
				t.Fatalf("broker 1: %v", err)
			}
			pub1 := httptest.NewServer(httptransport.NewPublisherHandler(b1, httptransport.Options{}))
			postMessage(t, pub1.URL, "/", msg)
			pub1.Close()
			if err := b1.Close(); err != nil {
				t.Fatalf("close broker 1: %v", err)
			}

			// 2. Start a completely fresh broker instance reading from the same file.
			b2, err := broker.New(broker.DefaultOptions(), makeStore(), nil)
			if err != nil {
				t.Fatalf("broker 2: %v", err)
			}
			defer b2.Close()

			sub2 := httptest.NewServer(httptransport.NewSubscriberHandler(b2, httptransport.Options{}))
			defer sub2.Close()

			// 3. Consume and verify message survived restart.
			got := getMessage(t, sub2.URL, "/")
			if got.ID != msg.ID || got.Type != msg.Type || string(got.Data) != string(msg.Data) {
				t.Fatalf("persistence mismatch: want %+v, got %+v", msg, got)
			}
		})
	}
}
