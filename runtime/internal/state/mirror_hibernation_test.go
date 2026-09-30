package state_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// A session that nobody is touching gives its terminal emulator back, and the
// next read is the same read it would have been.
func TestIdleSessionGivesBackItsEmulatorWithoutChangingItsSnapshot(t *testing.T) {
	root := t.TempDir()
	launcher := prototest.NewLauncher()
	registry := state.NewRegistry(state.Config{
		DefaultShell: "/bin/bash", DefaultCwd: root, DefaultCols: 300, DefaultRows: 50,
		RunnerStateDir: filepath.Join(root, "runners"), LaunchAgentsDir: filepath.Join(root, "agents"),
	}, launcher)
	ctx := context.Background()
	created, err := registry.Create(ctx, state.CreateSessionRequest{Cmd: "/bin/sh", Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	session, ok := registry.Get(created.ID)
	if !ok {
		t.Fatalf("session %s is not in the registry", created.ID)
	}
	runner := launcher.Runner(created.ID)
	runner.AddOutput("\x1b[32mbuild finished\x1b[0m\r\nready for the next turn\r\n")
	waitForMirror(t, func() bool { return session.OutputSeq() > 0 })

	before, _, err := session.Snapshot(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	terminalBefore, _, err := session.TerminalSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if hibernated := registry.HibernateIdleMirrors(0); hibernated != 1 {
		t.Fatalf("registry hibernated %d mirrors, want 1", hibernated)
	}

	after, _, err := session.Snapshot(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("snapshot after hibernation = %q, want %q", after, before)
	}
	terminalAfter, _, err := session.TerminalSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if terminalAfter != terminalBefore {
		t.Fatalf("terminal snapshot after hibernation = %q, want %q", terminalAfter, terminalBefore)
	}

	// Output that arrives while the session is resting lands where it would
	// have, because the write wakes the mirror before it is applied.
	runner.AddOutput("a new line after resting\r\n")
	waitForMirror(t, func() bool {
		text, _, snapErr := session.Snapshot(ctx, 0)
		return snapErr == nil && text != before
	})
	resumed, _, err := session.Snapshot(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := before + "\r\na new line after resting"; resumed != want {
		t.Fatalf("snapshot after waking = %q, want %q", resumed, want)
	}
}

func waitForMirror(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitConditionBudget)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("the session never reached the expected state (waited %s)", waitConditionBudget)
		}
		time.Sleep(waitConditionPoll)
	}
}
