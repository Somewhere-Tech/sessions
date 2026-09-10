package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// wakeFailingRegistry is the reboot-paused record that cannot get its runner
// back: the marker is durable, the wake attempt is real, and it fails. This is
// the shape a person hits after a restart when the runner binary, its launch
// service, or its socket is gone.
type wakeFailingRegistry struct {
	*state.Registry
	pending map[string]state.RestorePending
	wakes   []string
	err     error
}

func (r *wakeFailingRegistry) PendingRestore(id string) (state.RestorePending, bool) {
	pending, ok := r.pending[id]
	return pending, ok
}

func (r *wakeFailingRegistry) RestorePendingCount() int { return len(r.pending) }
func (r *wakeFailingRegistry) RetiredRestoreCount() int { return 0 }

func (r *wakeFailingRegistry) WakePaused(_ context.Context, id string) (state.SessionInfo, error) {
	r.wakes = append(r.wakes, id)
	return state.SessionInfo{}, r.err
}

// A read of a paused lane whose runner cannot be recreated must fail with the
// code and the exact command that recovers it. Answering 200 with an empty body
// is the failure this pins: it reads as "the session is fine and quiet", which
// is how a reboot turned into silence instead of a next step.
func TestReadsOfAPausedLaneThatCannotWakeStayActionable(t *testing.T) {
	daemon := newTestDaemon(t)
	const id = "11111111-2222-4333-8444-555555555555"
	registry := &wakeFailingRegistry{
		Registry: daemon.registry,
		pending: map[string]state.RestorePending{id: {
			SessionID: id, Reason: "bounded restart recovery paused this runner", DetectedAtMS: 123,
		}},
		err: errWakeUnavailable{},
	}
	handler := New(daemon.config, registry)

	for _, path := range []string{
		"/api/sessions/" + id + "/snapshot",
		"/api/sessions/" + id + "/events",
	} {
		response := serve(t, handler, http.MethodGet, path, nil, "127.0.0.1:1", nil)
		if response.Code != http.StatusConflict {
			t.Fatalf("GET %s = %d %s, want a refusal rather than a quiet read", path, response.Code, response.Body.String())
		}
		if strings.TrimSpace(response.Body.String()) == "" {
			t.Fatalf("GET %s answered with an empty body", path)
		}
		var body map[string]any
		decodeBody(t, response, &body)
		if body["code"] != "SESSION_NEEDS_RECREATE" || body["sessionId"] != id {
			t.Fatalf("GET %s body = %#v", path, body)
		}
		// The action names this lane, exactly, so the next step needs no
		// guessing from a title or a directory.
		if body["action"] != "sessions resume "+id {
			t.Fatalf("GET %s action = %#v", path, body["action"])
		}
		// The wake failure is the reason, not a generic pause message.
		if reason, _ := body["error"].(string); !strings.Contains(reason, "could not be restarted") ||
			!strings.Contains(reason, "cannot restart a paused session in place") {
			t.Fatalf("GET %s error = %#v", path, body["error"])
		}
	}
	if len(registry.wakes) != 2 {
		t.Fatalf("wake attempts = %v, want one per read", registry.wakes)
	}
}

// A lane that is merely paused, with no waker at all, keeps the same contract.
// The two paths differ only in what they say about why.
func TestPausedLaneWithoutAWakerKeepsTheSameActionableContract(t *testing.T) {
	daemon := newTestDaemon(t)
	const id = "22222222-3333-4444-8555-666666666666"
	handler := New(daemon.config, &pendingRestoreRegistry{
		Registry: daemon.registry,
		pending:  map[string]state.RestorePending{id: {SessionID: id, Reason: "paused by bounded restart recovery"}},
	})
	response := serve(t, handler, http.MethodGet, "/api/sessions/"+id+"/snapshot", nil, "127.0.0.1:1", nil)
	if response.Code != http.StatusConflict {
		t.Fatalf("paused snapshot = %d %s", response.Code, response.Body.String())
	}
	var body map[string]any
	decodeBody(t, response, &body)
	if body["code"] != "SESSION_NEEDS_RECREATE" || body["sessionId"] != id ||
		body["action"] != "sessions resume "+id {
		t.Fatalf("paused snapshot body = %#v", body)
	}
	if reason, _ := body["error"].(string); !strings.Contains(reason, "paused by bounded restart recovery") {
		t.Fatalf("paused snapshot reason = %#v", body["error"])
	}
}

// errWakeUnavailable reproduces the manager's own refusal text for a machine
// that cannot restart a paused runner in place.
type errWakeUnavailable struct{}

func (errWakeUnavailable) Error() string {
	return "this machine cannot restart a paused session in place; resume it with `sessions resume`"
}
