package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/AlbertMorenoDEV/simple-mq/internal/config"
)

func TestRunGracefulShutdown(t *testing.T) {
	cfg := config.Default()
	cfg.PubAddr = "127.0.0.1:0"
	cfg.SubAddr = "127.0.0.1:0"
	cfg.TCPAddr = "127.0.0.1:0"
	cfg.DataFile = filepath.Join(t.TempDir(), "queues.json")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	logger := buildLogger("error", "json")
	if err := run(ctx, cfg, logger); err != nil {
		t.Fatalf("expected clean graceful shutdown, got err=%v", err)
	}
}
