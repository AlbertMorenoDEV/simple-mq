package config

import (
	"testing"
	"time"
)

func TestLoadFlagsAndEnv(t *testing.T) {
	t.Setenv("SMQ_PUB_ADDR", "127.0.0.1:9001")
	t.Setenv("SMQ_STORE", "json")
	t.Setenv("SMQ_MAX_RETRIES", "5")

	cfg, err := Load([]string{"-tcp-addr", "127.0.0.1:9003", "-poll-timeout", "12s"})
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.PubAddr != "127.0.0.1:9001" {
		t.Fatalf("expected env PubAddr, got %q", cfg.PubAddr)
	}
	if cfg.StoreType != "json" {
		t.Fatalf("expected env StoreType json, got %q", cfg.StoreType)
	}
	if cfg.MaxRetries != 5 {
		t.Fatalf("expected env MaxRetries 5, got %d", cfg.MaxRetries)
	}
	if cfg.TCPAddr != "127.0.0.1:9003" {
		t.Fatalf("expected flag TCPAddr, got %q", cfg.TCPAddr)
	}
	if cfg.DefaultPollTimeout != 12*time.Second {
		t.Fatalf("expected flag DefaultPollTimeout 12s, got %v", cfg.DefaultPollTimeout)
	}
}

func TestValidateRejectsInvalidValues(t *testing.T) {
	if _, err := Load([]string{"-store", "invalid"}); err == nil {
		t.Fatalf("expected error for invalid store")
	}
	if _, err := Load([]string{"-delivery-mode", "bogus"}); err == nil {
		t.Fatalf("expected error for invalid delivery-mode")
	}
	if _, err := Load([]string{"-max-queue-depth", "0"}); err == nil {
		t.Fatalf("expected error for non-positive max-queue-depth")
	}
}
