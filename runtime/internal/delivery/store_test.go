package delivery

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testOperationID = "11111111-2222-4333-8444-555555555555"

func TestOperationIsDurableAndIdempotent(t *testing.T) {
	root := t.TempDir()
	store := New(root)
	clock := time.UnixMilli(100)
	store.now = func() time.Time { return clock }

	first, created, err := store.Begin(testOperationID, "session-a", "deploy once")
	if err != nil || !created || first.Status != StatusPending {
		t.Fatalf("Begin() = %+v, %t, %v", first, created, err)
	}
	clock = time.UnixMilli(200)
	accepted, err := store.Complete(testOperationID, StatusAccepted, true, false, "")
	if err != nil || accepted.Status != StatusAccepted || !accepted.Delivered || accepted.Retry {
		t.Fatalf("Complete() = %+v, %v", accepted, err)
	}

	restarted := New(root)
	again, created, err := restarted.Begin(testOperationID, "session-a", "deploy once")
	if err != nil || created || again.Status != StatusAccepted {
		t.Fatalf("idempotent Begin() = %+v, %t, %v", again, created, err)
	}
	if mode := fileMode(t, filepath.Join(root, "delivery-operations", testOperationID+".json")); mode != 0o600 {
		t.Fatalf("operation mode = %o, want 600", mode)
	}
}

func TestPendingOperationStaysUnknownAcrossRestart(t *testing.T) {
	root := t.TempDir()
	if _, created, err := New(root).Begin(testOperationID, "session-a", "maybe delivered"); err != nil || !created {
		t.Fatalf("Begin() created=%t err=%v", created, err)
	}
	record, created, err := New(root).Begin(testOperationID, "session-a", "maybe delivered")
	if err != nil || created || record.Status != StatusPending || record.Retry {
		t.Fatalf("pending retry = %+v, %t, %v", record, created, err)
	}
}

func TestOperationIDCannotBeReusedForDifferentMessage(t *testing.T) {
	store := New(t.TempDir())
	if _, _, err := store.Begin(testOperationID, "session-a", "first"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Begin(testOperationID, "session-a", "second"); err == nil {
		t.Fatal("different content reused an operation id")
	}
	if _, _, err := store.Begin(testOperationID, "session-b", "first"); err == nil {
		t.Fatal("different target reused an operation id")
	}
}

func TestInvalidOperationIDCannotEscapeRoot(t *testing.T) {
	root := t.TempDir()
	if _, _, err := New(root).Begin("../../escape", "session", "text"); err == nil {
		t.Fatal("unsafe operation id accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "escape.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe path exists: %v", err)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
