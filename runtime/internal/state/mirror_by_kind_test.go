package state_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// What each session kind answers on every read that could reach the terminal
// mirror.
//
// The daemon feeds PTY output into a mirror for every kind, but only some kinds
// ever read one back: a lane answers from its raw output tail and a structured
// provider from its event log, both before the mirror is consulted. This test
// is the enumeration — one row per kind per read — so that stopping the writes
// for the kinds that never read is a change with a record of what it cannot
// affect.

const ptyOutput = "\x1b[32mgreen\x1b[0m line\r\nsecond line\r\n"

type kindReads struct {
	snapshot         string
	reflowed         string
	terminalSnapshot string
	replayed         string
	snapshotErr      error
}

func readEveryPath(t *testing.T, session *state.Session) kindReads {
	t.Helper()
	ctx := context.Background()
	var reads kindReads
	var err error
	// `sessions snap`, `sessions tail`, the WS snapshot RPC and the idle
	// classifier all arrive here with no columns.
	reads.snapshot, _, err = session.Snapshot(ctx, 0)
	reads.snapshotErr = err
	// The reflowed view a phone asks for: GET /api/sessions/{id}/snapshot?cols=N.
	reads.reflowed, _, _ = session.Snapshot(ctx, 100)
	// The terminal view: GET .../snapshot?scrollback=1.
	reads.terminalSnapshot, _, _ = session.TerminalSnapshot(ctx)
	// What a WS client is replayed when it attaches.
	attachment := session.Attach(state.AttachOptions{})
	defer attachment.Cancel()
	var replayed strings.Builder
	for _, event := range attachment.Replay.Events {
		replayed.WriteString(event.Data)
	}
	reads.replayed = replayed.String()
	return reads
}

func sessionOfKind(t *testing.T, kind string) (*state.Session, *prototest.Runner) {
	t.Helper()
	root := t.TempDir()
	launcher := prototest.NewLauncher()
	registry := state.NewRegistry(state.Config{
		DefaultShell: "/bin/bash", DefaultCwd: root, DefaultCols: 300, DefaultRows: 50,
		RunnerStateDir: filepath.Join(root, "runners"), LaunchAgentsDir: filepath.Join(root, "agents"),
	}, launcher)
	// The command decides the tool, and a structured kind is only accepted for
	// the provider it belongs to.
	command := "/bin/sh"
	switch kind {
	case state.KindCodexAppServer:
		command = "/usr/local/bin/codex"
	case state.KindClaudeStructured:
		command = "/usr/local/bin/claude"
	}
	created, err := registry.Create(context.Background(), state.CreateSessionRequest{
		Cmd: command, Cwd: root, Kind: kind, Name: "kind " + kind,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, ok := registry.Get(created.ID)
	if !ok {
		t.Fatalf("session %s is not in the registry", created.ID)
	}
	runner := launcher.Runner(created.ID)
	runner.AddOutput(ptyOutput)
	waitForMirror(t, func() bool { return session.OutputSeq() > 0 })
	if kind == state.KindCodexAppServer || kind == state.KindClaudeStructured {
		event, marshalErr := json.Marshal(map[string]any{
			"type":    "assistant",
			"message": map[string]any{"role": "assistant", "content": "structured answer"},
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		session.RecordClaudeEvent(event)
	}
	return session, runner
}

func TestEveryReadPathByKind(t *testing.T) {
	for _, test := range []struct {
		kind string
		// what each read answers with, described rather than pasted, so a
		// change of substance is visible and a change of spacing is not.
		wants func(t *testing.T, reads kindReads)
	}{
		{
			kind: "",
			wants: func(t *testing.T, reads kindReads) {
				// A terminal session is the only kind whose reads come from the
				// mirror: the text is the rendered screen, not the raw stream.
				if !strings.Contains(reads.snapshot, "green") || strings.Contains(reads.snapshot, "\x1b[32m\x1b[0m") {
					t.Errorf("terminal snapshot = %q", reads.snapshot)
				}
				if !strings.Contains(reads.reflowed, "second line") {
					t.Errorf("terminal reflow = %q", reads.reflowed)
				}
				if !strings.Contains(reads.terminalSnapshot, "second line") {
					t.Errorf("terminal scrollback snapshot = %q", reads.terminalSnapshot)
				}
			},
		},
		{
			kind: state.KindLane,
			wants: func(t *testing.T, reads kindReads) {
				// A lane answers every read with its raw output tail, byte for
				// byte, including the escape sequences.
				if reads.snapshot != ptyOutput {
					t.Errorf("lane snapshot = %q, want the raw tail %q", reads.snapshot, ptyOutput)
				}
				if reads.reflowed != ptyOutput {
					t.Errorf("lane reflow = %q, want the raw tail", reads.reflowed)
				}
				if reads.terminalSnapshot != ptyOutput {
					t.Errorf("lane scrollback snapshot = %q, want the raw tail", reads.terminalSnapshot)
				}
			},
		},
		{
			kind: state.KindCodexAppServer,
			wants: func(t *testing.T, reads kindReads) {
				for name, got := range map[string]string{
					"snapshot": reads.snapshot, "reflow": reads.reflowed,
					"scrollback snapshot": reads.terminalSnapshot,
				} {
					if !strings.Contains(got, "structured answer") || strings.Contains(got, "green") {
						t.Errorf("codex-app-server %s = %q, want the structured event log", name, got)
					}
				}
			},
		},
		{
			kind: state.KindClaudeStructured,
			wants: func(t *testing.T, reads kindReads) {
				for name, got := range map[string]string{
					"snapshot": reads.snapshot, "reflow": reads.reflowed,
					"scrollback snapshot": reads.terminalSnapshot,
				} {
					if !strings.Contains(got, "structured answer") || strings.Contains(got, "green") {
						t.Errorf("claude-structured %s = %q, want the structured event log", name, got)
					}
				}
			},
		},
	} {
		name := test.kind
		if name == "" {
			name = "terminal"
		}
		t.Run(name, func(t *testing.T) {
			session, _ := sessionOfKind(t, test.kind)
			reads := readEveryPath(t, session)
			if reads.snapshotErr != nil {
				t.Fatalf("Snapshot: %v", reads.snapshotErr)
			}
			// Every kind replays the same raw output on attach: the WS replay
			// is the event log, never the mirror.
			if reads.replayed != ptyOutput {
				t.Errorf("attach replay = %q, want the raw output %q", reads.replayed, ptyOutput)
			}
			test.wants(t, reads)

			// Resizing is not a read, but it runs through the same field. A
			// kind with no screen still records the size its runner took.
			if !session.Resize(context.Background(), 120, 40) {
				t.Fatal("Resize reported failure")
			}
			if info := session.Info(); info.Cols != 120 || info.Rows != 40 {
				t.Errorf("after Resize the session reports %dx%d, want 120x40", info.Cols, info.Rows)
			}
		})
	}
}
