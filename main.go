package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AlbertMorenoDEV/simple-mq/internal/broker"
	"github.com/AlbertMorenoDEV/simple-mq/internal/config"
	"github.com/AlbertMorenoDEV/simple-mq/internal/storage"
	httptransport "github.com/AlbertMorenoDEV/simple-mq/internal/transport/http"
	tcptransport "github.com/AlbertMorenoDEV/simple-mq/internal/transport/tcp"
)

func main() {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
		os.Exit(2)
	}

	logger := buildLogger(cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	store, err := buildStore(cfg)
	if err != nil {
		return fmt.Errorf("init storage: %w", err)
	}

	mode, err := broker.ParseDeliveryMode(cfg.DeliveryMode, broker.ModeHybrid)
	if err != nil {
		return err
	}

	b, err := broker.New(broker.Options{
		DeliveryMode:      mode,
		MaxQueueDepth:     cfg.MaxQueueDepth,
		MaxQueues:         cfg.MaxQueues,
		VisibilityTimeout: cfg.VisibilityTimeout,
		MaxRetries:        cfg.MaxRetries,
	}, store, logger)
	if err != nil {
		return fmt.Errorf("init broker: %w", err)
	}

	httpOpts := httptransport.Options{
		MaxBodyBytes:       cfg.MaxMessageBytes,
		DefaultPollTimeout: cfg.DefaultPollTimeout,
		AuthToken:          cfg.AuthToken,
		Logger:             logger,
	}
	pubSrv := httptransport.NewHTTPServer(cfg.PubAddr, httptransport.NewPublisherHandler(b, httpOpts))
	subSrv := httptransport.NewHTTPServer(cfg.SubAddr, httptransport.NewSubscriberHandler(b, httpOpts))
	tcpSrv := tcptransport.NewServer(b, tcptransport.Options{
		MaxMessageBytes:    cfg.MaxMessageBytes,
		DefaultPollTimeout: cfg.DefaultPollTimeout,
		IdleTimeout:        cfg.TCPIdleTimeout,
		AuthToken:          cfg.AuthToken,
		Logger:             logger,
	})

	errCh := make(chan error, 3)
	var wg sync.WaitGroup

	wg.Add(3)
	go func() {
		defer wg.Done()
		logger.Info("publisher HTTP server starting", "addr", cfg.PubAddr)
		if err := pubSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("publisher server: %w", err)
		}
	}()

	go func() {
		defer wg.Done()
		logger.Info("subscriber HTTP server starting", "addr", cfg.SubAddr)
		if err := subSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("subscriber server: %w", err)
		}
	}()

	go func() {
		defer wg.Done()
		if err := tcpSrv.ListenAndServe(cfg.TCPAddr); err != nil {
			errCh <- fmt.Errorf("tcp server: %w", err)
		}
	}()

	var runErr error
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining servers...")
	case err := <-errCh:
		runErr = err
		logger.Error("fatal listener error, initiating shutdown", "err", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := pubSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("publisher server shutdown error", "err", err)
	}
	if err := subSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("subscriber server shutdown error", "err", err)
	}
	if err := tcpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("tcp server shutdown error", "err", err)
	}

	wg.Wait()

	if err := b.Close(); err != nil {
		logger.Error("broker close error", "err", err)
		if runErr == nil {
			runErr = err
		}
	}

	logger.Info("simple-mq stopped cleanly")
	return runErr
}

func buildStore(cfg config.Config) (storage.Store, error) {
	switch strings.ToLower(cfg.StoreType) {
	case "wal":
		return storage.NewWALStore(cfg.DataFile, cfg.WALCompactEvery)
	case "json":
		return storage.NewJSONStore(cfg.DataFile), nil
	case "memory":
		return storage.NewMemoryStore(), nil
	default:
		return nil, fmt.Errorf("unsupported store type %q", cfg.StoreType)
	}
}

func buildLogger(levelStr, format string) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(levelStr) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	if strings.ToLower(format) == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}
