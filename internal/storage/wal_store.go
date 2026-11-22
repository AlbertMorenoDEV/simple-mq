package storage

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/AlbertMorenoDEV/simple-mq/internal/model"
)

type walOp string

const (
	walOpPublish walOp = "publish"
	walOpConsume walOp = "consume"
	walOpRequeue walOp = "requeue"
	walOpPurge   walOp = "purge"
)

type walEntry struct {
	Op      walOp         `json:"op"`
	Queue   string        `json:"queue"`
	MsgID   string        `json:"msg_id,omitempty"`
	Message model.Message `json:"message,omitempty"`
	Delayed bool          `json:"delayed,omitempty"`
}

// WALStore combines an append-only NDJSON write-ahead log (O(1) per mutation)
// with atomic JSON snapshot compaction into snapshotPath.
type WALStore struct {
	snapshotPath string
	walPath      string
	compactEvery int

	mu       sync.Mutex
	walFile  *os.File
	walCount int
}

// NewWALStore creates a WALStore using snapshotPath for compacted snapshots and
// snapshotPath+".wal" for the append-only write-ahead log.
func NewWALStore(snapshotPath string, compactEvery int) (*WALStore, error) {
	if compactEvery <= 0 {
		compactEvery = 1000
	}
	dir := filepath.Dir(snapshotPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create storage dir: %w", err)
		}
	}
	return &WALStore{
		snapshotPath: snapshotPath,
		walPath:      snapshotPath + ".wal",
		compactEvery: compactEvery,
	}, nil
}

func (w *WALStore) ensureOpenLocked() error {
	if w.walFile != nil {
		return nil
	}
	f, err := os.OpenFile(w.walPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open wal %s: %w", w.walPath, err)
	}
	w.walFile = f
	return nil
}

// Load loads the latest snapshot and replays any uncompacted WAL entries on top.
func (w *WALStore) Load() (*model.Snapshot, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	snap, err := loadSnapshotFile(w.snapshotPath)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(w.walPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return snap, nil
		}
		return nil, fmt.Errorf("open wal for replay %s: %w", w.walPath, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Allow WAL lines up to 16 MiB.
	scanner.Buffer(make([]byte, 0, 64*1024), 16<<20)

	replayed := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry walEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			// Stop cleanly on a partial trailing line written right before a crash.
			break
		}
		applyWALEntry(snap, entry)
		replayed++
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan wal %s: %w", w.walPath, err)
	}
	w.walCount = replayed
	return snap, nil
}

func applyWALEntry(snap *model.Snapshot, e walEntry) {
	switch e.Op {
	case walOpPublish:
		if e.Delayed {
			snap.Delayed[e.Queue] = append(snap.Delayed[e.Queue], e.Message)
		} else {
			snap.Queues[e.Queue] = append(snap.Queues[e.Queue], e.Message)
		}
	case walOpConsume:
		if list, ok := snap.Queues[e.Queue]; ok {
			snap.Queues[e.Queue] = removeByID(list, e.MsgID)
			if len(snap.Queues[e.Queue]) == 0 {
				delete(snap.Queues, e.Queue)
			}
		}
		if list, ok := snap.Delayed[e.Queue]; ok {
			snap.Delayed[e.Queue] = removeByID(list, e.MsgID)
			if len(snap.Delayed[e.Queue]) == 0 {
				delete(snap.Delayed, e.Queue)
			}
		}
	case walOpRequeue:
		// Deduplicate if already present, then push to front.
		list := removeByID(snap.Queues[e.Queue], e.Message.ID)
		snap.Queues[e.Queue] = append([]model.Message{e.Message}, list...)
	case walOpPurge:
		delete(snap.Queues, e.Queue)
		delete(snap.Delayed, e.Queue)
	}
}

func removeByID(list []model.Message, id string) []model.Message {
	for i, m := range list {
		if m.ID == id {
			return append(list[:i], list[i+1:]...)
		}
	}
	return list
}

func (w *WALStore) appendEntry(e walEntry) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.ensureOpenLocked(); err != nil {
		return err
	}

	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal wal entry: %w", err)
	}
	line = append(line, '\n')

	if _, err := w.walFile.Write(line); err != nil {
		return fmt.Errorf("write wal entry: %w", err)
	}
	if err := w.walFile.Sync(); err != nil {
		return fmt.Errorf("sync wal: %w", err)
	}
	w.walCount++
	return nil
}

func (w *WALStore) RecordPublish(queue string, msg model.Message, delayed bool) error {
	return w.appendEntry(walEntry{
		Op:      walOpPublish,
		Queue:   queue,
		Message: msg,
		Delayed: delayed,
	})
}

func (w *WALStore) RecordConsume(queue, msgID string) error {
	return w.appendEntry(walEntry{
		Op:    walOpConsume,
		Queue: queue,
		MsgID: msgID,
	})
}

func (w *WALStore) RecordRequeue(queue string, msg model.Message) error {
	return w.appendEntry(walEntry{
		Op:      walOpRequeue,
		Queue:   queue,
		Message: msg,
	})
}

func (w *WALStore) RecordPurge(queue string) error {
	return w.appendEntry(walEntry{
		Op:    walOpPurge,
		Queue: queue,
	})
}

func (w *WALStore) NeedsCompaction() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.walCount >= w.compactEvery
}

// SaveSnapshot writes a full atomic snapshot and resets the WAL file.
func (w *WALStore) SaveSnapshot(snap *model.Snapshot) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}
	if err := atomicWriteFile(w.snapshotPath, data, 0o644); err != nil {
		return err
	}

	if w.walFile != nil {
		_ = w.walFile.Close()
		w.walFile = nil
	}
	if err := os.Remove(w.walPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove compacted wal: %w", err)
	}
	w.walCount = 0
	return nil
}

func (w *WALStore) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.walFile != nil {
		err := w.walFile.Close()
		w.walFile = nil
		return err
	}
	return nil
}
