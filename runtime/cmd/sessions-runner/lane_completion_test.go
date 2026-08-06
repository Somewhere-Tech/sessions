package main

import (
	"bytes"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func newLaneTestRunner(t *testing.T, paths state.Paths) *runner {
	t.Helper()
	return &runner{
		cfg:        config{id: paths.ID, kind: state.KindLane, cmd: "./deploy.sh", cwd: "/tmp", specPath: "deploy.yaml"},
		paths:      paths,
		createdAt:  time.Now().Add(-time.Minute).UnixMilli(),
		process:    &durabilityProcess{output: bytes.NewReader(nil)},
		log:        state.NewEventLog(state.DefaultEventCap),
		persistent: &stubHistory{},
		logger:     log.New(io.Discard, "", 0),
		clients:    make(map[*client]struct{}),
		readDone:   make(chan struct{}),
	}
}

func TestInterruptedLaneRecordsATerminalOutcomeThatIsNotSuccess(t *testing.T) {
	paths := state.For(t.TempDir(), "lane-session")
	r := newLaneTestRunner(t, paths)
	r.log.Push([]byte("migrating table 3 of 40\r\n"))

	r.writeLaneManifest(r.interruptedManifest)

	manifest, err := state.ReadCompletionManifest(paths.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ExitCode == 0 {
		t.Fatal("an interrupted lane reported a clean exit code")
	}
	if manifest.Signal == nil || *manifest.Signal != "SIGTERM" {
		t.Fatalf("interrupted lane signal = %#v", manifest.Signal)
	}
	if !strings.Contains(manifest.LastOutputTail, "stopped by shutdown") ||
		!strings.Contains(manifest.LastOutputTail, "start it again") {
		t.Fatalf("interrupted lane tail does not explain the outcome: %q", manifest.LastOutputTail)
	}
	if !strings.Contains(manifest.LastOutputTail, "migrating table 3 of 40") {
		t.Fatalf("interrupted lane tail dropped the command's own output: %q", manifest.LastOutputTail)
	}
	if manifest.SpecPath != "deploy.yaml" {
		t.Fatalf("interrupted lane spec path = %q", manifest.SpecPath)
	}
}

func TestFirstLaneRecordWins(t *testing.T) {
	paths := state.For(t.TempDir(), "lane-session")
	r := newLaneTestRunner(t, paths)
	code := 0

	r.writeLaneManifest(func() state.CompletionManifest { return r.completionManifest(exitInfo{Code: &code}) })
	// Shutdown arriving after the child already finished must not replace the
	// real outcome with an interrupted one.
	r.writeLaneManifest(r.interruptedManifest)

	manifest, err := state.ReadCompletionManifest(paths.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ExitCode != 0 || manifest.Signal != nil {
		t.Fatalf("completed lane record was overwritten: %#v", manifest)
	}
}

func TestOnlyLanesRecordCompletion(t *testing.T) {
	paths := state.For(t.TempDir(), "pty-session")
	r := newLaneTestRunner(t, paths)
	r.cfg.kind = ""

	r.writeLaneManifest(r.interruptedManifest)

	if _, err := os.Stat(paths.Manifest); !os.IsNotExist(err) {
		t.Fatalf("a plain PTY session wrote a lane completion record: %v", err)
	}
}

func TestCompletedLaneDoesNotRunItsCommandAgain(t *testing.T) {
	paths := state.For(t.TempDir(), "lane-session")
	cfg := config{id: paths.ID, kind: state.KindLane, cmd: "./deploy.sh"}
	logger := log.New(io.Discard, "", 0)

	if code, guarded := guardCompletedLane(cfg, paths, logger); guarded {
		t.Fatalf("a lane with no completion record was blocked from running (code %d)", code)
	}

	signal := "SIGTERM"
	if err := state.WriteCompletionManifest(paths.Manifest, state.CompletionManifest{
		ExitCode: laneInterruptedExitCode, Signal: &signal,
	}); err != nil {
		t.Fatal(err)
	}
	code, guarded := guardCompletedLane(cfg, paths, logger)
	if !guarded {
		t.Fatal("a lane with an existing completion record was allowed to run its command again")
	}
	// launchd restarts a runner that exits non-zero; a guarded lane must not
	// turn into a restart loop.
	if code != 0 {
		t.Fatalf("guarded lane exit code = %d, want a clean exit", code)
	}
	if _, err := state.ReadCompletionManifest(paths.Manifest); err != nil {
		t.Fatalf("the existing lane record did not survive the guard: %v", err)
	}
}

func TestNonLaneRunnerIsNeverBlockedByALaneRecord(t *testing.T) {
	paths := state.For(t.TempDir(), "pty-session")
	if err := state.WriteCompletionManifest(paths.Manifest, state.CompletionManifest{}); err != nil {
		t.Fatal(err)
	}
	if _, guarded := guardCompletedLane(config{id: paths.ID, cmd: "bash"}, paths, log.New(io.Discard, "", 0)); guarded {
		t.Fatal("a plain PTY session was blocked by a stray lane record")
	}
}
