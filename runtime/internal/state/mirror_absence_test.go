package state

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
)

// The kinds that keep a terminal screen, and the answer for one that does not.
func TestKindKeepsTerminalMirror(t *testing.T) {
	for kind, want := range map[string]bool{
		"":                   true,
		KindLane:             false,
		KindCodexAppServer:   false,
		KindClaudeStructured: false,
		"a-kind-added-later": true,
	} {
		if got := kindKeepsTerminalMirror(kind); got != want {
			t.Errorf("kindKeepsTerminalMirror(%q) = %v, want %v", kind, got, want)
		}
	}
}

// A session with no screen and nothing to answer with says so. No kind reaches
// this today — a lane answers from its output and a structured provider from
// its events — but a kind added to the no-mirror list without a reader of its
// own would, and an empty screen would read as a session with nothing on it.
func TestSnapshotOfAKindWithNoScreenSaysSo(t *testing.T) {
	root := t.TempDir()
	launcher := prototest.NewLauncher()
	registry := NewRegistry(Config{
		DefaultShell: "/bin/bash", DefaultCwd: root, DefaultCols: 300, DefaultRows: 50,
		RunnerStateDir: filepath.Join(root, "runners"), LaunchAgentsDir: filepath.Join(root, "agents"),
	}, launcher)
	created, err := registry.Create(context.Background(), CreateSessionRequest{Cmd: "/bin/sh", Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	session, ok := registry.Get(created.ID)
	if !ok {
		t.Fatalf("session %s is not in the registry", created.ID)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	session.mirror = nil
	session.mu.Unlock()

	for _, read := range []struct {
		name string
		call func() (string, uint32, error)
	}{
		{"snapshot", func() (string, uint32, error) { return session.Snapshot(context.Background(), 0) }},
		{"reflow", func() (string, uint32, error) { return session.Snapshot(context.Background(), 100) }},
		{"scrollback", func() (string, uint32, error) { return session.TerminalSnapshot(context.Background()) }},
	} {
		text, _, err := read.call()
		if !errors.Is(err, ErrNoTerminalMirror) {
			t.Errorf("%s error = %v, want ErrNoTerminalMirror", read.name, err)
		}
		if text != "" {
			t.Errorf("%s text = %q, want empty", read.name, text)
		}
	}
	// Everything else a session does still works without a screen.
	if !session.Resize(context.Background(), 120, 40) {
		t.Error("Resize on a session with no screen reported failure")
	}
	if !session.HibernateIdleMirror(0) {
		t.Error("a session with no screen reports an emulator to give back")
	}
	if err := session.Close(); err != nil {
		t.Errorf("Close on a session with no screen: %v", err)
	}
}
