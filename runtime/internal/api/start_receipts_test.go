package api

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/delivery"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

const (
	apiStartOperation  = "5c000000-0000-4000-8000-000000000001"
	apiPromptOperation = "5c000000-0000-4000-8000-000000000002"
)

func TestUnavailableDeliveryStoreDoesNotProveNothingWasSent(t *testing.T) {
	server := &Server{}
	info := server.withStartReceipt(state.SessionInfo{ID: "worker", Start: &state.StartReceipt{
		OperationID: apiStartOperation, PromptOperationID: apiPromptOperation,
	}})
	if info.Start.Phase != state.StartPhasePromptUnknown || info.Start.Prompt.Retry || info.Start.Recovery.Action != state.StartRecoveryInspect {
		t.Fatalf("missing delivery store implied safe resend: %+v", info.Start)
	}
}

type startDaemon struct {
	testDaemon
	manager *sessionruntime.Manager
	store   *ledger.Store
}

func newStartDaemon(t *testing.T) startDaemon {
	t.Helper()
	daemon := newTestDaemon(t)
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(daemon.root, "ledger", "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := sessionruntime.NewManager(daemon.config, daemon.launcher, sessionruntime.ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
	})
	t.Cleanup(manager.Close)
	daemon.handler = New(daemon.config, manager)
	return startDaemon{testDaemon: daemon, manager: manager, store: store}
}

func (d startDaemon) create(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	response := serve(t, d.handler, http.MethodPost, "/api/sessions", strings.NewReader(body), "127.0.0.1:1", nil)
	var decoded map[string]any
	decodeBody(t, response, &decoded)
	return response.Code, decoded
}

func shellStartBody(root string) string {
	return `{"cmd":"/bin/sh","cwd":"` + root + `","operation_id":"` + apiStartOperation + `","prompt_operation_id":"` + apiPromptOperation + `"}`
}

// Creation can succeed while the first request never goes out. The retry
// returns the same session, the listing says the request was not sent and how
// to send it safely, and delivering it moves the receipt forward.
func TestStartReceiptFollowsCreateReplayAndFirstRequest(t *testing.T) {
	daemon := newStartDaemon(t)
	status, first := daemon.create(t, shellStartBody(daemon.root))
	if status != http.StatusCreated {
		t.Fatalf("create = %d %#v", status, first)
	}
	id, _ := first["id"].(string)
	start, _ := first["start"].(map[string]any)
	if start["phase"] != state.StartPhaseCreated || start["prompt"].(map[string]any)["status"] != state.StartPromptNotSent {
		t.Fatalf("create start receipt = %#v", start)
	}
	status, again := daemon.create(t, shellStartBody(daemon.root))
	if status != http.StatusOK || again["id"] != id || again["start"].(map[string]any)["replayed"] != true {
		t.Fatalf("replayed create = %d %#v, want 200 for %s", status, again, id)
	}
	if launches := len(daemon.launcher.Launches); launches != 1 {
		t.Fatalf("launches = %d, want 1", launches)
	}
	listed := sessionInfoOf(t, daemon.handler, id)
	if listed.Start == nil || listed.Start.Recovery == nil || listed.Start.Recovery.Action != state.StartRecoverySendPrompt ||
		!strings.Contains(listed.Start.Recovery.Command, apiPromptOperation) {
		t.Fatalf("not-sent recovery = %#v", listed.Start)
	}

	done := daemon.handler.beginDeliveryInFlight(apiPromptOperation)
	if sending := sessionInfoOf(t, daemon.handler, id); sending.Start.Prompt.Status != state.StartPromptSending {
		t.Fatalf("in-flight prompt = %#v", sending.Start.Prompt)
	}
	done()

	receipt := submitMessageRequest(t, daemon.handler, id, apiPromptOperation, "run the tests")
	if receipt["status"] != "accepted" {
		t.Fatalf("first request receipt = %#v", receipt)
	}
	delivered := sessionInfoOf(t, daemon.handler, id)
	if delivered.Start.Phase != state.StartPhasePromptDelivered || delivered.Start.Prompt.Status != state.StartPromptAccepted {
		t.Fatalf("delivered start = %#v", delivered.Start)
	}
	runner := daemon.launcher.Runner(id)
	inputs := len(runner.Inputs())
	if duplicate := submitMessageRequest(t, daemon.handler, id, apiPromptOperation, "run the tests"); duplicate["duplicate"] != true {
		t.Fatalf("second submit of the first request = %#v", duplicate)
	}
	if len(runner.Inputs()) != inputs {
		t.Fatalf("resubmitting the first request wrote input again: %#v", runner.Inputs())
	}
}

// An ambiguous delivery is reported as unknown with an inspect recovery, and
// asking again reads the receipt instead of sending the request a second time.
func TestStartReceiptAmbiguousDeliveryIsNeverResent(t *testing.T) {
	daemon := newStartDaemon(t)
	_, created := daemon.create(t, shellStartBody(daemon.root))
	id := created["id"].(string)
	if _, _, err := daemon.handler.deliveries.Begin(apiPromptOperation, id, "run the tests"); err != nil {
		t.Fatal(err)
	}
	if _, err := daemon.handler.deliveries.Complete(apiPromptOperation, delivery.StatusUnknown, false, false, "request context deadline exceeded"); err != nil {
		t.Fatal(err)
	}
	listed := sessionInfoOf(t, daemon.handler, id)
	if listed.Start.Phase != state.StartPhasePromptUnknown || listed.Start.Recovery.Action != state.StartRecoveryInspect ||
		!strings.Contains(listed.Start.Evidence, "deadline exceeded") {
		t.Fatalf("ambiguous start = %#v", listed.Start)
	}
	before := len(daemon.launcher.Runner(id).Inputs())
	if again := submitMessageRequest(t, daemon.handler, id, apiPromptOperation, "run the tests"); again["status"] != "unknown" || again["duplicate"] != true {
		t.Fatalf("retry of ambiguous request = %#v", again)
	}
	if after := len(daemon.launcher.Runner(id).Inputs()); after != before {
		t.Fatalf("ambiguous first request was resent: %d → %d inputs", before, after)
	}
}

// A provider that rejects the login is a blocked start that names Connect
// account, not an idle session, and Sessions does not clear the fault.
func TestStartReceiptBlockedByProviderAuthentication(t *testing.T) {
	daemon := newStartDaemon(t)
	status, created := daemon.create(t, `{"cmd":"claude","cwd":"`+daemon.root+`","kind":"claude-structured","operation_id":"`+apiStartOperation+`"}`)
	if status != http.StatusCreated {
		t.Fatalf("create = %d %#v", status, created)
	}
	id := created["id"].(string)
	daemon.launcher.Runner(id).AddClaudeEvent(map[string]any{
		"type": "system", "subtype": "provider_fault", "provider": "claude", "kind": "auth",
		"detail": "Claude is not signed in", "evidence": "Invalid API key · Please run /login",
	})
	blocked := waitForSession(t, daemon.handler, id, func(info state.SessionInfo) bool { return info.FailureKind == "auth" })
	if blocked.Start == nil || blocked.Start.Phase != state.StartPhaseBlocked || blocked.Start.BlockedBy != "auth" ||
		blocked.Start.Recovery == nil || blocked.Start.Recovery.Action != state.StartRecoveryConnectAccount ||
		!strings.Contains(blocked.Start.Evidence, "Please run /login") {
		t.Fatalf("auth start receipt = %#v", blocked.Start)
	}
	if again := sessionInfoOf(t, daemon.handler, id); again.FailureKind != "auth" {
		t.Fatalf("listing cleared the provider fault: %#v", again)
	}
}

// An id whose session has ended is answered with that session, never with a
// second runtime.
func TestCreateReplayOfEndedSessionIsConflictNamingTheSession(t *testing.T) {
	daemon := newStartDaemon(t)
	_, created := daemon.create(t, shellStartBody(daemon.root))
	id := created["id"].(string)
	if !daemon.manager.Kill(context.Background(), id, false) {
		t.Fatal("kill was not accepted")
	}
	waitForExit(t, daemon.manager, id)
	status, body := daemon.create(t, shellStartBody(daemon.root))
	if status != http.StatusConflict || body["session_id"] != id || body["operation_id"] != apiStartOperation {
		t.Fatalf("replay of ended session = %d %#v", status, body)
	}
	if len(daemon.launcher.Launches) != 1 {
		t.Fatalf("launches = %d, want 1", len(daemon.launcher.Launches))
	}
}

// The receipt is rebuilt from durable state after a daemon restart: the ids
// from the ledger, the first request from its delivery receipt.
func TestStartReceiptSurvivesDaemonRestart(t *testing.T) {
	daemon := newStartDaemon(t)
	_, created := daemon.create(t, shellStartBody(daemon.root))
	id := created["id"].(string)
	if receipt := submitMessageRequest(t, daemon.handler, id, apiPromptOperation, "run the tests"); receipt["status"] != "accepted" {
		t.Fatalf("first request = %#v", receipt)
	}
	daemon.manager.Close()

	restarted := sessionruntime.NewManager(daemon.config, daemon.launcher, sessionruntime.ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: daemon.store.Boundaries(), Observations: daemon.store.Observations(), LedgerReader: daemon.store,
	})
	t.Cleanup(restarted.Close)
	if err := restarted.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	handler := New(daemon.config, restarted)
	response := serve(t, handler, http.MethodGet, "/api/sessions?include_exited=1", nil, "127.0.0.1:1", nil)
	var listed struct {
		Sessions []state.SessionInfo `json:"sessions"`
	}
	decodeBody(t, response, &listed)
	for _, info := range listed.Sessions {
		if info.ID != id {
			continue
		}
		if info.Start == nil || info.Start.PromptOperationID != apiPromptOperation || info.Start.Prompt == nil || info.Start.Prompt.Status != state.StartPromptAccepted {
			t.Fatalf("restarted start receipt = %#v", info.Start)
		}
		return
	}
	t.Fatalf("session %s missing after restart: %#v", id, listed.Sessions)
}

// The ledger records a session before its runner launches. When the launch
// then fails, the answer keeps that session's id, and a retry under the same
// operation id names it instead of launching another.
func TestCreateFailureAfterRecordingKeepsTheSessionID(t *testing.T) {
	daemon := newStartDaemon(t)
	daemon.launcher.Err = errors.New("runner did not create socket within 60s")
	status, body := daemon.create(t, shellStartBody(daemon.root))
	sessionID, _ := body["session_id"].(string)
	recovery, _ := body["recovery"].(map[string]any)
	if status != http.StatusInternalServerError || sessionID == "" || body["operation_id"] != apiStartOperation ||
		recovery["action"] != state.StartRecoveryInspect || !strings.Contains(body["error"].(string), "socket") {
		t.Fatalf("failed create = %d %#v", status, body)
	}
	daemon.launcher.Err = nil
	status, again := daemon.create(t, shellStartBody(daemon.root))
	if status != http.StatusConflict || again["session_id"] != sessionID || again["operation_id"] != apiStartOperation ||
		!strings.Contains(again["error"].(string), "never reported ready") || !strings.Contains(again["error"].(string), "inspect the recorded runner") {
		t.Fatalf("retry after failed launch = %d %#v", status, again)
	}
	if len(daemon.launcher.Launches) != 0 {
		t.Fatalf("launches = %d, want none", len(daemon.launcher.Launches))
	}
}

// waitForExit holds until the killed session has actually exited: Kill only
// accepts the request, and a replay before the exit rightly returns the
// still-live session.
func waitForExit(t *testing.T, manager *sessionruntime.Manager, id string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if current, ok := manager.Get(id); !ok || current.Info().Exited {
			return
		}
	}
	t.Fatalf("session %s never exited", id)
}
