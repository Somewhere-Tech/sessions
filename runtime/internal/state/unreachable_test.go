package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
)

// proto.SocketRunner raises EventRunnerLost for any socket read error at all:
// EOF, the 10s read deadline, a daemon restart. None of those observed the
// process. Recording one as an exit told the user their session had ended and
// failed -- exited, idle reason "failed", nil exit code -- about a provider
// that was still working, and tore down everything the session had spawned
// behind that claim.
//
// A lost socket makes a session unreachable. Nothing more.
func TestRunnerLostMarksUnreachableAndNotExited(t *testing.T) {
	session, launcher, id := newLostRunnerSession(t)

	launcher.Runner(id).AddOutput("work that really happened\n")
	launcher.Runner(id).Emit(proto.Event{Kind: proto.EventRunnerLost})
	awaitInfo(t, session, "unreachable", func(info SessionInfo) bool { return info.Unreachable })

	info := session.Info()
	if info.Exited {
		t.Fatal("a lost socket was recorded as an exit; Exited must mean a reaped status")
	}
	if info.ExitCode != nil || info.ExitSignal != nil || info.ExitReason != "" || info.ExitedAt != nil {
		t.Fatalf("a lost socket invented exit details: %#v", info)
	}
	if info.IdleReason == IdleReasonFailed {
		t.Fatal("a lost socket was reported as a failure")
	}
	if !info.Unreachable {
		t.Fatal("a lost socket did not mark the session unreachable")
	}
	if info.UnreachableReason != "runner-lost" || info.UnreachableSince == nil {
		t.Fatalf("unreachable state is missing its reason or time: %#v", info)
	}
	if info.Working {
		t.Fatal("a session with no socket cannot be observed working")
	}

	// A read is served from durable history and needs no process at all. An
	// unreachable session stays attachable, and the websocket path must not
	// close it as an ended terminal.
	if exited, _ := session.TerminalState(); exited {
		t.Fatal("an unreachable session reported a terminal exit state to attachers")
	}
	attachment := session.Attach(AttachOptions{})
	defer attachment.Cancel()
	replayed := ""
	for _, event := range attachment.Replay.Events {
		replayed += event.Data
	}
	if replayed != "work that really happened\n" {
		t.Fatalf("attach to an unreachable session replayed %q", replayed)
	}
}

// A real exit still is one, with every exit detail intact.
func TestRunnerExitIsStillAnExit(t *testing.T) {
	session, launcher, id := newLostRunnerSession(t)

	code := 3
	launcher.Runner(id).Emit(proto.Event{
		Kind: proto.EventExit,
		Exit: proto.ExitEvent{Code: &code, Reason: "failed"},
	})
	awaitInfo(t, session, "exited", func(info SessionInfo) bool { return info.Exited })
	info := session.Info()
	if !info.Exited || info.ExitCode == nil || *info.ExitCode != 3 {
		t.Fatalf("a reaped status was not recorded as an exit: %#v", info)
	}
	if info.IdleReason != IdleReasonFailed {
		t.Fatalf("a non-zero exit code idle reason = %q", info.IdleReason)
	}
	if info.Unreachable {
		t.Fatal("an exited session was also marked unreachable")
	}
}

func newLostRunnerSession(t *testing.T) (*Session, *prototest.Launcher, string) {
	t.Helper()
	root := t.TempDir()
	config := Config{
		DefaultShell: "/bin/bash", DefaultCwd: root, DefaultCols: 300, DefaultRows: 50,
		RunnerStateDir: filepath.Join(root, "runners"), LaunchAgentsDir: filepath.Join(root, "agents"),
	}
	launcher := prototest.NewLauncher()
	registry := NewRegistry(config, launcher)
	created, err := registry.Create(context.Background(), CreateSessionRequest{Cmd: "/bin/sh", Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	session, ok := registry.Get(created.ID)
	if !ok {
		t.Fatal("created session was not registered")
	}
	return session, launcher, created.ID
}

// awaitInfo polls the session's own view rather than a runner-side signal: the
// event pump is what this test is about, so the test must not depend on it
// having already run.
func awaitInfo(t *testing.T, session *Session, want string, satisfied func(SessionInfo) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if satisfied(session.Info()) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("session never became %s: %#v", want, session.Info())
}
