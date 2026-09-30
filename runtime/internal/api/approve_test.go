package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func sessionInfoOf(t *testing.T, handler http.Handler, id string) state.SessionInfo {
	t.Helper()
	response := serve(t, handler, http.MethodGet, "/api/sessions", nil, "127.0.0.1:1", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("session list status=%d body=%s", response.Code, response.Body.String())
	}
	var listed struct {
		Sessions []state.SessionInfo `json:"sessions"`
	}
	decodeBody(t, response, &listed)
	for _, info := range listed.Sessions {
		if info.ID == id {
			return info
		}
	}
	t.Fatalf("session %s missing from the list", id)
	return state.SessionInfo{}
}

func waitForSession(t *testing.T, handler http.Handler, id string, ready func(state.SessionInfo) bool) state.SessionInfo {
	t.Helper()
	var info state.SessionInfo
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		info = sessionInfoOf(t, handler, id)
		if ready(info) {
			return info
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session never reached the expected state: %#v", info)
	return info
}

// A Rich Codex lane that is not autonomous asks before acting. The request
// arrives as a structured event, the session reads needs-you with the request
// as its line, and POST /approve answers it through the runner.
func TestRichCodexApprovalRoutesThroughSessions(t *testing.T) {
	daemon := newTestDaemon(t)
	manager := sessionruntime.NewManager(daemon.config, daemon.launcher, sessionruntime.ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
	})
	t.Cleanup(manager.Close)
	daemon.handler = New(daemon.config, manager)

	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "codex", Cwd: daemon.root, Kind: state.KindCodexAppServer,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := daemon.launcher.Runner(created.ID)
	path := "/api/sessions/" + created.ID + "/approve"

	idle := serve(t, daemon.handler, http.MethodPost, path, strings.NewReader(`{"decision":"allow"}`), "127.0.0.1:1", nil)
	if idle.Code != http.StatusConflict {
		t.Fatalf("approve with nothing waiting status=%d body=%s", idle.Code, idle.Body.String())
	}

	runner.AddCodexEvent(map[string]any{
		"type": "codex", "subtype": "turn_started", "source": "codex-app-server", "conversationId": "thread-1", "turnId": "turn-1",
	})
	waitForSession(t, daemon.handler, created.ID, func(info state.SessionInfo) bool { return info.Working })
	runner.AddCodexEvent(map[string]any{
		"type": "system", "subtype": "approval_requested", "source": "codex-app-server", "conversationId": "thread-1", "turnId": "turn-1",
		"approval": map[string]any{"id": "approval-1", "kind": "command", "summary": "Run `npm test`", "command": "npm test", "cwd": daemon.root},
	})
	waiting := waitForSession(t, daemon.handler, created.ID, func(info state.SessionInfo) bool {
		return !info.Working && info.PendingApproval != nil && info.IdleReason == state.IdleReasonNeedsInput
	})
	if waiting.PendingApproval.ID != "approval-1" || waiting.PendingApproval.Command != "npm test" || waiting.IdleDetail != "Allow? Run `npm test`" {
		t.Fatalf("waiting session = %#v detail=%q", waiting.PendingApproval, waiting.IdleDetail)
	}

	bad := serve(t, daemon.handler, http.MethodPost, path, strings.NewReader(`{"decision":"later"}`), "127.0.0.1:1", nil)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("bad decision status=%d body=%s", bad.Code, bad.Body.String())
	}
	wrong := serve(t, daemon.handler, http.MethodPost, path, strings.NewReader(`{"decision":"allow","id":"approval-9"}`), "127.0.0.1:1", nil)
	if wrong.Code != http.StatusConflict {
		t.Fatalf("wrong id status=%d body=%s", wrong.Code, wrong.Body.String())
	}

	headers := http.Header{}
	headers.Set("X-Sessions-Creator-Session", "manager-1")
	answered := serve(t, daemon.handler, http.MethodPost, path, strings.NewReader(`{"decision":"allow-session"}`), "127.0.0.1:1", headers)
	if answered.Code != http.StatusOK || !strings.Contains(answered.Body.String(), `"approval-1"`) {
		t.Fatalf("approve status=%d body=%s", answered.Code, answered.Body.String())
	}
	controls := runner.Approvals()
	if len(controls) != 1 || controls[0].ID != "approval-1" || controls[0].Decision != "allow-session" || controls[0].By != "manager-1" {
		t.Fatalf("runner approvals = %#v", controls)
	}

	runner.AddCodexEvent(map[string]any{
		"type": "system", "subtype": "approval_resolved", "source": "codex-app-server", "conversationId": "thread-1",
		"approval": map[string]any{"id": "approval-1", "decision": "allow-session", "by": "manager-1"},
	})
	resumed := waitForSession(t, daemon.handler, created.ID, func(info state.SessionInfo) bool {
		return info.Working && info.PendingApproval == nil
	})
	if resumed.IdleReason != "" {
		t.Fatalf("resumed session still idle: %#v", resumed)
	}
}

// The Claude path carries the same contract: the permission-prompt shim's
// request becomes an approval_requested event, and the answer travels back
// over the same Approve frame.
func TestRichClaudeApprovalRoutesThroughSessions(t *testing.T) {
	daemon := newTestDaemon(t)
	manager := sessionruntime.NewManager(daemon.config, daemon.launcher, sessionruntime.ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
	})
	t.Cleanup(manager.Close)
	daemon.handler = New(daemon.config, manager)
	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "claude", Cwd: daemon.root, Kind: state.KindClaudeStructured,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := daemon.launcher.Runner(created.ID)
	runner.AddCodexEvent(map[string]any{"type": "claude", "subtype": "turn_started", "source": "claude-p-stream-json", "session_id": "s-1"})
	waitForSession(t, daemon.handler, created.ID, func(info state.SessionInfo) bool { return info.Working })
	runner.AddCodexEvent(map[string]any{
		"type": "system", "subtype": "approval_requested", "source": "claude-p-stream-json", "session_id": "s-1",
		"approval": map[string]any{"id": "approval-1", "kind": "command", "summary": "Run `touch a.txt`", "command": "touch a.txt", "tool": "Bash"},
	})
	waiting := waitForSession(t, daemon.handler, created.ID, func(info state.SessionInfo) bool {
		return !info.Working && info.PendingApproval != nil && info.IdleReason == state.IdleReasonNeedsInput
	})
	if waiting.IdleDetail != "Allow? Run `touch a.txt`" {
		t.Fatalf("waiting detail = %q", waiting.IdleDetail)
	}
	answered := serve(t, daemon.handler, http.MethodPost, "/api/sessions/"+created.ID+"/approve", strings.NewReader(`{"decision":"deny"}`), "127.0.0.1:1", nil)
	if answered.Code != http.StatusOK {
		t.Fatalf("approve status=%d body=%s", answered.Code, answered.Body.String())
	}
	if controls := runner.Approvals(); len(controls) != 1 || controls[0].Decision != "deny" || controls[0].By != "" {
		t.Fatalf("runner approvals = %#v", controls)
	}
	runner.AddCodexEvent(map[string]any{
		"type": "system", "subtype": "approval_resolved", "source": "claude-p-stream-json", "session_id": "s-1",
		"approval": map[string]any{"id": "approval-1", "decision": "deny", "by": ""},
	})
	waitForSession(t, daemon.handler, created.ID, func(info state.SessionInfo) bool { return info.Working && info.PendingApproval == nil })
}
