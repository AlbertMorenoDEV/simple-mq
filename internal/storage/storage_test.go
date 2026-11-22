package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/AlbertMorenoDEV/simple-mq/internal/model"
)

func TestJSONStoreRoundTripAndLegacyLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queues.json")

	// 1. Verify legacy v0.2 flat map format loads cleanly.
	legacy := map[string][]model.Message{
		"default": {
			{ID: "1", Type: "legacy", Data: json.RawMessage(`"hello"`)},
		},
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	store := NewJSONStore(path)
	snap, err := store.Load()
	if err != nil {
		t.Fatalf("legacy load failed: %v", err)
	}
	if len(snap.Queues["default"]) != 1 || snap.Queues["default"][0].ID != "1" {
		t.Fatalf("unexpected legacy snapshot: %+v", snap)
	}

	// 2. Save v0.3 atomic snapshot and reload.
	snap.Queues["tasks"] = []model.Message{
		{ID: "2", Type: "job", Data: json.RawMessage(`{"k":"v"}`)},
	}
	if err := store.SaveSnapshot(snap); err != nil {
		t.Fatalf("save snapshot failed: %v", err)
	}

	reloaded, err := store.Load()
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if len(reloaded.Queues["tasks"]) != 1 {
		t.Fatalf("unexpected reloaded tasks: %+v", reloaded.Queues["tasks"])
	}
	var gotData map[string]string
	if err := json.Unmarshal(reloaded.Queues["tasks"][0].Data, &gotData); err != nil || gotData["k"] != "v" {
		t.Fatalf("unexpected reloaded task data: %s (err=%v)", string(reloaded.Queues["tasks"][0].Data), err)
	}
}

func TestWALStoreReplayCompactionAndCrashTolerance(t *testing.T) {
	dir := t.TempDir()
	snapPath := filepath.Join(dir, "queues.json")

	w, err := NewWALStore(snapPath, 5)
	if err != nil {
		t.Fatal(err)
	}

	m1 := model.Message{ID: "m1", Type: "t", Data: json.RawMessage(`"one"`)}
	m2 := model.Message{ID: "m2", Type: "t", Data: json.RawMessage(`"two"`)}
	m3 := model.Message{ID: "m3", Type: "t", Data: json.RawMessage(`"three"`)}

	if err := w.RecordPublish("q1", m1, false); err != nil {
		t.Fatal(err)
	}
	if err := w.RecordPublish("q1", m2, false); err != nil {
		t.Fatal(err)
	}
	if err := w.RecordConsume("q1", "m1"); err != nil {
		t.Fatal(err)
	}
	if err := w.RecordPublish("q1", m3, true); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate partial trailing line from a power loss mid-write.
	f, err := os.OpenFile(snapPath+".wal", os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"op":"publish","queue":"q1","message":{"id":"corrupt`)
	_ = f.Close()

	// Replay into a fresh WALStore instance.
	w2, err := NewWALStore(snapPath, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()

	snap, err := w2.Load()
	if err != nil {
		t.Fatalf("WAL replay failed: %v", err)
	}
	if len(snap.Queues["q1"]) != 1 || snap.Queues["q1"][0].ID != "m2" {
		t.Fatalf("expected only m2 in q1, got %+v", snap.Queues["q1"])
	}
	if len(snap.Delayed["q1"]) != 1 || snap.Delayed["q1"][0].ID != "m3" {
		t.Fatalf("expected m3 in delayed q1, got %+v", snap.Delayed["q1"])
	}

	// Compact and confirm WAL file is removed and snapshot reloads identically.
	if err := w2.SaveSnapshot(snap); err != nil {
		t.Fatalf("compaction failed: %v", err)
	}
	if _, err := os.Stat(snapPath + ".wal"); !os.IsNotExist(err) {
		t.Fatalf("expected WAL file to be removed after compaction, err=%v", err)
	}

	w3, _ := NewWALStore(snapPath, 5)
	defer w3.Close()
	snap3, err := w3.Load()
	if err != nil {
		t.Fatalf("post-compaction load failed: %v", err)
	}
	if len(snap3.Queues["q1"]) != 1 || snap3.Queues["q1"][0].ID != "m2" {
		t.Fatalf("unexpected post-compaction state: %+v", snap3.Queues["q1"])
	}
}
