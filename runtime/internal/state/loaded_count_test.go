package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
)

// Health's session count must answer while a session is busy: a runner frame
// in flight holds that session's lock, and the count is about the registry,
// not about any one session.
func TestLoadedSessionCountTakesNoSessionLock(t *testing.T) {
	root := t.TempDir()
	registry := NewRegistry(Config{
		DefaultShell: "/bin/bash", DefaultCwd: root, DefaultCols: 300, DefaultRows: 50,
		RunnerStateDir: filepath.Join(root, "runners"), LaunchAgentsDir: filepath.Join(root, "agents"),
	}, prototest.NewLauncher())
	created, err := registry.Create(context.Background(), CreateSessionRequest{Cmd: "/bin/sh", Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	session, ok := registry.Get(created.ID)
	if !ok {
		t.Fatalf("session %s is not in the registry", created.ID)
	}
	t.Cleanup(func() { _ = session.Close() })

	session.mu.Lock()
	counted := make(chan int, 1)
	go func() { counted <- registry.LoadedSessionCount() }()
	select {
	case count := <-counted:
		session.mu.Unlock()
		if count != 1 {
			t.Fatalf("loaded sessions = %d, want 1", count)
		}
	case <-time.After(2 * time.Second):
		session.mu.Unlock()
		<-counted
		t.Fatal("counting loaded sessions waited for a session's lock")
	}
	if listed := len(registry.List(true)); listed != registry.LoadedSessionCount() {
		t.Fatalf("count %d disagrees with the registry listing %d", registry.LoadedSessionCount(), listed)
	}
}
