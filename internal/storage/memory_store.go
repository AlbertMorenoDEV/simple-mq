package storage

import "github.com/AlbertMorenoDEV/simple-mq/internal/model"

// MemoryStore is a no-op persistence backend for ephemeral in-memory operation.
type MemoryStore struct{}

// NewMemoryStore returns a new MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{}
}

func (m *MemoryStore) Load() (*model.Snapshot, error) {
	return &model.Snapshot{
		Queues:  make(map[string][]model.Message),
		Delayed: make(map[string][]model.Message),
	}, nil
}

func (m *MemoryStore) RecordPublish(string, model.Message, bool) error { return nil }
func (m *MemoryStore) RecordConsume(string, string) error              { return nil }
func (m *MemoryStore) RecordRequeue(string, model.Message) error       { return nil }
func (m *MemoryStore) RecordPurge(string) error                        { return nil }
func (m *MemoryStore) NeedsCompaction() bool                           { return false }
func (m *MemoryStore) SaveSnapshot(*model.Snapshot) error              { return nil }
func (m *MemoryStore) Close() error                                    { return nil }
