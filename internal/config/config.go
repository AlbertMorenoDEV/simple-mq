package config

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime settings for simple-mq.
type Config struct {
	PubAddr            string
	SubAddr            string
	TCPAddr            string
	DataFile           string
	StoreType          string // "wal", "json", or "memory"
	DeliveryMode       string // "hybrid", "queue", or "fanout"
	MaxMessageBytes    int64
	MaxQueueDepth      int
	MaxQueues          int
	DefaultPollTimeout time.Duration
	TCPIdleTimeout     time.Duration
	VisibilityTimeout  time.Duration
	MaxRetries         int
	WALCompactEvery    int
	AuthToken          string
	LogLevel           string
	LogFormat          string // "text" or "json"
}

// Default returns a Config populated with production-safe defaults that
// remain backwards-compatible with simple-mq's default ports and data file.
func Default() Config {
	return Config{
		PubAddr:            ":8001",
		SubAddr:            ":8002",
		TCPAddr:            ":8003",
		DataFile:           "queues.json",
		StoreType:          "wal",
		DeliveryMode:       "hybrid",
		MaxMessageBytes:    1 << 20, // 1 MiB
		MaxQueueDepth:      100_000,
		MaxQueues:          10_000,
		DefaultPollTimeout: 30 * time.Second,
		TCPIdleTimeout:     5 * time.Minute,
		VisibilityTimeout:  30 * time.Second,
		MaxRetries:         3,
		WALCompactEvery:    1000,
		AuthToken:          "",
		LogLevel:           "info",
		LogFormat:          "text",
	}
}

// Load parses configuration from environment variables (SMQ_*) and then
// overlays CLI flags from args.
func Load(args []string) (Config, error) {
	cfg := Default()
	applyEnv(&cfg)

	fs := flag.NewFlagSet("simple-mq", flag.ContinueOnError)
	fs.StringVar(&cfg.PubAddr, "pub-addr", cfg.PubAddr, "Publisher/Admin HTTP listen address (env: SMQ_PUB_ADDR)")
	fs.StringVar(&cfg.SubAddr, "sub-addr", cfg.SubAddr, "Subscriber HTTP listen address (env: SMQ_SUB_ADDR)")
	fs.StringVar(&cfg.TCPAddr, "tcp-addr", cfg.TCPAddr, "TCP socket listen address (env: SMQ_TCP_ADDR)")
	fs.StringVar(&cfg.DataFile, "data-file", cfg.DataFile, "Persistence snapshot file path (env: SMQ_DATA_FILE)")
	fs.StringVar(&cfg.StoreType, "store", cfg.StoreType, "Storage backend: wal, json, memory (env: SMQ_STORE)")
	fs.StringVar(&cfg.DeliveryMode, "delivery-mode", cfg.DeliveryMode, "Default delivery mode: hybrid, queue, fanout (env: SMQ_DELIVERY_MODE)")
	fs.Int64Var(&cfg.MaxMessageBytes, "max-message-bytes", cfg.MaxMessageBytes, "Maximum message/request payload size in bytes (env: SMQ_MAX_MESSAGE_BYTES)")
	fs.IntVar(&cfg.MaxQueueDepth, "max-queue-depth", cfg.MaxQueueDepth, "Maximum messages per queue (env: SMQ_MAX_QUEUE_DEPTH)")
	fs.IntVar(&cfg.MaxQueues, "max-queues", cfg.MaxQueues, "Maximum number of distinct queues (env: SMQ_MAX_QUEUES)")
	fs.DurationVar(&cfg.DefaultPollTimeout, "poll-timeout", cfg.DefaultPollTimeout, "Default long-poll timeout (env: SMQ_POLL_TIMEOUT)")
	fs.DurationVar(&cfg.TCPIdleTimeout, "tcp-idle-timeout", cfg.TCPIdleTimeout, "Idle read timeout for TCP connections (env: SMQ_TCP_IDLE_TIMEOUT)")
	fs.DurationVar(&cfg.VisibilityTimeout, "visibility-timeout", cfg.VisibilityTimeout, "Default visibility timeout for ACK mode (env: SMQ_VISIBILITY_TIMEOUT)")
	fs.IntVar(&cfg.MaxRetries, "max-retries", cfg.MaxRetries, "Max delivery attempts before routing to DLQ (env: SMQ_MAX_RETRIES)")
	fs.IntVar(&cfg.WALCompactEvery, "wal-compact-every", cfg.WALCompactEvery, "WAL entries before triggering snapshot compaction (env: SMQ_WAL_COMPACT_EVERY)")
	fs.StringVar(&cfg.AuthToken, "auth-token", cfg.AuthToken, "Optional shared bearer token for authentication (env: SMQ_AUTH_TOKEN)")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "Log level: debug, info, warn, error (env: SMQ_LOG_LEVEL)")
	fs.StringVar(&cfg.LogFormat, "log-format", cfg.LogFormat, "Log format: text, json (env: SMQ_LOG_FORMAT)")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("SMQ_PUB_ADDR"); v != "" {
		cfg.PubAddr = v
	}
	if v := os.Getenv("SMQ_SUB_ADDR"); v != "" {
		cfg.SubAddr = v
	}
	if v := os.Getenv("SMQ_TCP_ADDR"); v != "" {
		cfg.TCPAddr = v
	}
	if v := os.Getenv("SMQ_DATA_FILE"); v != "" {
		cfg.DataFile = v
	}
	if v := os.Getenv("SMQ_STORE"); v != "" {
		cfg.StoreType = strings.ToLower(v)
	}
	if v := os.Getenv("SMQ_DELIVERY_MODE"); v != "" {
		cfg.DeliveryMode = strings.ToLower(v)
	}
	if v := os.Getenv("SMQ_MAX_MESSAGE_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.MaxMessageBytes = n
		}
	}
	if v := os.Getenv("SMQ_MAX_QUEUE_DEPTH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxQueueDepth = n
		}
	}
	if v := os.Getenv("SMQ_MAX_QUEUES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxQueues = n
		}
	}
	if v := os.Getenv("SMQ_POLL_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.DefaultPollTimeout = d
		}
	}
	if v := os.Getenv("SMQ_TCP_IDLE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.TCPIdleTimeout = d
		}
	}
	if v := os.Getenv("SMQ_VISIBILITY_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.VisibilityTimeout = d
		}
	}
	if v := os.Getenv("SMQ_MAX_RETRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxRetries = n
		}
	}
	if v := os.Getenv("SMQ_WAL_COMPACT_EVERY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.WALCompactEvery = n
		}
	}
	if v := os.Getenv("SMQ_AUTH_TOKEN"); v != "" {
		cfg.AuthToken = v
	}
	if v := os.Getenv("SMQ_LOG_LEVEL"); v != "" {
		cfg.LogLevel = strings.ToLower(v)
	}
	if v := os.Getenv("SMQ_LOG_FORMAT"); v != "" {
		cfg.LogFormat = strings.ToLower(v)
	}
}

// Validate ensures all configuration values are within valid bounds.
func (c Config) Validate() error {
	switch strings.ToLower(c.StoreType) {
	case "wal", "json", "memory":
	default:
		return fmt.Errorf("invalid store type %q: must be wal, json, or memory", c.StoreType)
	}

	switch strings.ToLower(c.DeliveryMode) {
	case "hybrid", "queue", "fanout":
	default:
		return fmt.Errorf("invalid delivery mode %q: must be hybrid, queue, or fanout", c.DeliveryMode)
	}

	if c.MaxMessageBytes <= 0 {
		return fmt.Errorf("max-message-bytes must be positive")
	}
	if c.MaxQueueDepth <= 0 {
		return fmt.Errorf("max-queue-depth must be positive")
	}
	if c.MaxQueues <= 0 {
		return fmt.Errorf("max-queues must be positive")
	}
	if c.VisibilityTimeout <= 0 {
		return fmt.Errorf("visibility-timeout must be positive")
	}
	if c.MaxRetries <= 0 {
		return fmt.Errorf("max-retries must be positive")
	}
	switch strings.ToLower(c.LogFormat) {
	case "text", "json":
	default:
		return fmt.Errorf("invalid log format %q: must be text or json", c.LogFormat)
	}
	return nil
}
