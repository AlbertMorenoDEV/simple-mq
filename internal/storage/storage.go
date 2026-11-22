package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AlbertMorenoDEV/simple-mq/internal/model"
)

// Store defines the persistence contract used by the broker.
type Store interface {
	Load() (*model.Snapshot, error)
	RecordPublish(queue string, msg model.Message, delayed bool) error
	RecordConsume(queue, msgID string) error
	RecordRequeue(queue string, msg model.Message) error
	RecordPurge(queue string) error
	NeedsCompaction() bool
	SaveSnapshot(snap *model.Snapshot) error
	Close() error
}

// atomicWriteFile writes data to a temporary file in the same directory as path,
// fsyncs it, and atomically renames it into place so crashes never corrupt the file.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create dir %s: %w", dir, err)
		}
	}

	tmp, err := os.CreateTemp(dir, ".simple-mq-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp file to %s: %w", path, err)
	}
	cleanup = false
	return nil
}

// loadSnapshotFile reads a JSON snapshot file, supporting both the v0.3 Snapshot
// envelope ({"queues": {...}, "delayed": {...}}) and the legacy v0.2 flat map
// format ({"default": [...]}).
func loadSnapshotFile(path string) (*model.Snapshot, error) {
	snap := &model.Snapshot{
		Queues:  make(map[string][]model.Message),
		Delayed: make(map[string][]model.Message),
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return snap, nil
		}
		return nil, fmt.Errorf("read snapshot %s: %w", path, err)
	}
	if len(data) == 0 {
		return snap, nil
	}

	// Detect format by inspecting top-level keys.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal snapshot %s: %w", path, err)
	}

	if qRaw, hasQueues := raw["queues"]; hasQueues {
		// Check whether "queues" is a JSON object (v0.3 envelope) vs array (a queue named "queues" in legacy v0.2).
		trimmed := trimLeadingSpace(qRaw)
		if len(trimmed) > 0 && (trimmed[0] == '{' || string(trimmed) == "null") {
			var parsed model.Snapshot
			if err := json.Unmarshal(data, &parsed); err != nil {
				return nil, fmt.Errorf("unmarshal v0.3 snapshot %s: %w", path, err)
			}
			if parsed.Queues != nil {
				snap.Queues = parsed.Queues
			}
			if parsed.Delayed != nil {
				snap.Delayed = parsed.Delayed
			}
			return snap, nil
		}
	}

	// Legacy v0.2 format: map[string][]model.Message
	var legacy map[string][]model.Message
	if err := json.Unmarshal(data, &legacy); err != nil {
		return nil, fmt.Errorf("unmarshal legacy snapshot %s: %w", path, err)
	}
	if legacy != nil {
		snap.Queues = legacy
	}
	return snap, nil
}

func trimLeadingSpace(b []byte) []byte {
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
		i++
	}
	return b[i:]
}
