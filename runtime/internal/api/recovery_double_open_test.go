package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

const doubleOpenConversation = "3fe0b590-6916-41cc-a9b8-3b6c4e75fa17"

// writeAdoptableClaudeConversation puts a resumable Claude conversation where
// the resolver looks for one, so these tests differ only in who else has it
// open.
func writeAdoptableClaudeConversation(t *testing.T, home, cwd, conversation string) {
	t.Helper()
	path := filepath.Join(home, ".claude", "projects", watch.EncodeClaudeCWD(cwd), conversation+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"user","uuid":"u1","timestamp":"2026-08-01T10:00:00Z",` +
		`"message":{"role":"user","content":"where did the auth fix land"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeClaudeLiveEntry writes one ~/.claude/sessions/<pid>.json in the shape
// Claude Code actually uses, copied from real files on a live machine.
func writeClaudeLiveEntry(t *testing.T, home string, pid int, conversation, name, cwd string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", watch.ClaudeLiveRegistryDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(map[string]any{
		"pid": pid, "sessionId": conversation, "cwd": cwd, "name": name,
		"status": "waiting", "waitingFor": "permission prompt",
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, strconv.Itoa(pid)+".json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A conversation open in the user's own terminal is the case Sessions cannot
// prevent and must not pretend to: the resume proceeds, and the response says
// where else it is open so the caller can pass that on. Nothing set
// AdoptOptions.ClaudeLive before this, so alsoOpenIn was always empty and the
// second window was silent.
func TestResumeReportsTheOtherWindowTheConversationIsOpenIn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	daemon := newTestDaemon(t)
	writeAdoptableClaudeConversation(t, home, daemon.root, doubleOpenConversation)
	// The test process is a live pid that the manager is not running anything
	// as, which is exactly the shape of a Claude the user started themselves.
	writeClaudeLiveEntry(t, home, os.Getpid(), doubleOpenConversation, "pretty-pty-02", daemon.root)

	body := strings.NewReader(`{"target":"` + doubleOpenConversation + `"}`)
	response := serve(
		t, daemon.handler, http.MethodPost, "/api/recovery/adopt", body, "127.0.0.1:1", nil,
	)
	if response.Code != http.StatusCreated {
		t.Fatalf("resume status=%d body=%s — reporting a second window must never gate the resume",
			response.Code, response.Body.String())
	}
	var result recovery.AdoptResult
	decodeBody(t, response, &result)
	if !result.OK || result.LaneID == "" {
		t.Fatalf("result = %+v, want the resume to have proceeded", result)
	}
	if _, live := daemon.registry.Get(result.LaneID); !live {
		t.Fatalf("session %s was not started", result.LaneID)
	}
	for _, want := range []string{"pretty-pty-02", fmt.Sprintf("pid %d", os.Getpid()), "permission prompt"} {
		if !strings.Contains(result.AlsoOpenIn, want) {
			t.Fatalf("alsoOpenIn = %q, want it to name %q so the caller can say where else it is open",
				result.AlsoOpenIn, want)
		}
	}
	if !strings.Contains(response.Body.String(), `"alsoOpenIn"`) {
		t.Fatalf("resume response body carries no alsoOpenIn field: %s", response.Body.String())
	}
}

// Ownership comes from the manager's live session list, never from the daemon's
// own process tree. Runners are started through launchd, so the Claude process
// under one is not a descendant of the daemon; seeding ownership with the
// daemon's pid resolves nothing and reports every conversation Sessions is
// itself running as somebody else's window. Here the manager is running a
// session as the very pid the registry entry names -- the shape a live machine
// shows, where the manager's row for a Sessions-launched Claude carries that
// Claude's pid directly -- so there is nothing to report.
func TestResumeStaysQuietWhenSessionsIsTheOneRunningTheConversation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	daemon := newTestDaemon(t)
	daemon.launcher.PID = os.Getpid()
	if _, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "claude", Cwd: daemon.root, Name: "already running here",
	}); err != nil {
		t.Fatal(err)
	}
	writeAdoptableClaudeConversation(t, home, daemon.root, doubleOpenConversation)
	writeClaudeLiveEntry(t, home, os.Getpid(), doubleOpenConversation, "already running here", daemon.root)

	body := strings.NewReader(`{"target":"` + doubleOpenConversation + `"}`)
	response := serve(
		t, daemon.handler, http.MethodPost, "/api/recovery/adopt", body, "127.0.0.1:1", nil,
	)
	if response.Code != http.StatusCreated {
		t.Fatalf("resume status=%d body=%s", response.Code, response.Body.String())
	}
	var result recovery.AdoptResult
	decodeBody(t, response, &result)
	if result.AlsoOpenIn != "" {
		t.Fatalf("alsoOpenIn = %q, want silence: this process is one of Sessions' own runners",
			result.AlsoOpenIn)
	}
}

// The seed is the manager's live rows. An exited row's pid has already been
// reused by the operating system, so keeping it would eventually vouch for a
// stranger's process; a row with no pid vouches for nothing.
func TestOwnedRunnerPIDsAreTheManagersLiveRows(t *testing.T) {
	pids := ownedRunnerPIDs([]state.SessionInfo{
		{ID: "live", PID: 4321},
		{ID: "exited", PID: 4322, Exited: true},
		{ID: "unlaunched", PID: 0},
	})
	if len(pids) != 1 || pids[0] != 4321 {
		t.Fatalf("ownedRunnerPIDs = %v, want only the live row's pid", pids)
	}
}

// A profile keeps its own CLAUDE_CONFIG_DIR, and Claude writes the live
// registry beside the projects tree under that root. Looking a profile
// conversation up in the default ~/.claude registry would examine a different
// set of processes entirely.
func TestClaudeLiveQueryFollowsTheProfileRoot(t *testing.T) {
	query := claudeLiveQuery(nil, "/Users/someone/.claude-work/projects")
	if query.Dir != filepath.Join("/Users/someone/.claude-work", watch.ClaudeLiveRegistryDirName) {
		t.Fatalf("registry dir = %q, want the profile's own registry", query.Dir)
	}
	if defaulted := claudeLiveQuery(nil, ""); defaulted.Dir != "" {
		t.Fatalf("registry dir = %q, want the real default resolved at read time", defaulted.Dir)
	}
}
