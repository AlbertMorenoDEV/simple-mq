package storage

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/AlbertMorenoDEV/simple-mq/internal/model"
)

// JSONStore persists full queue snapshots atomically to a JSON file.
type JSONStore struct {
	path string
	mu   sync.Mutex
}

// NewJSONStore creates a JSONStore targeting path.
func NewJSONStore(path string) *JSONStore {
	return &JSONStore{path: path}
}

func (s *JSONStore) Load() (*model.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return loadSnapshotFile(s.path)
}

func (s *JSONStore) RecordPublish(string, model.Message, bool) error { return nil }
func (s *JSONStore) RecordConsume(string, string) error              { return nil }
func (s *JSONStore) RecordRequeue(string, model.Message) error       { return nil }
func (s *JSONStore) RecordPurge(string) error                        { return nil }

// NeedsCompaction always returns true so every state-changing operation
// persists an atomic snapshot immediately.
func (s *JSONStore) NeedsCompaction() bool { return true }

func (s *JSONStore) SaveSnapshot(snap *model.Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}
	return atomicWriteFile(s.path, data, 0o644)
}

func (s *JSONStore) Close() error { return nil }
