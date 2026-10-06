package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const startSessionID = "cccccccc-dddd-4eee-8fff-000000000001"

// startDaemon is a fake sessionsd that behaves like the real one for create
// replay: the first create with an operation id records it and its prompt id,
// and a repeat returns the same session with the recorded prompt id.
type startDaemon struct {
	mu            sync.Mutex
	createBodies  []map[string]any
	submitOps     []string
	promptOp      string
	submitReceipt map[string]any
	start         map[string]any
	// listed adds fields to the listing's record, so it can differ from the
	// create answer the way a session that has since started does.
	listed map[string]any
	// legacy answers like a daemon from before start receipts: it ignores
	// operation ids and returns the session without a start object.
	legacy bool
}

func (d *startDaemon) serve(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		d.mu.Lock()
		defer d.mu.Unlock()
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/api/sessions":
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			d.createBodies = append(d.createBodies, body)
			start := map[string]any{"operation_id": body["operation_id"], "phase": "created"}
			status := http.StatusCreated
			if d.promptOp == "" {
				d.promptOp, _ = body["prompt_operation_id"].(string)
			} else {
				start["replayed"] = true
				status = http.StatusOK
			}
			start["prompt_operation_id"] = d.promptOp
			created := map[string]any{"id": startSessionID, "cmd": "claude", "kind": "claude-structured", "start": start,
				"working": false, "idleReason": "never-started"}
			if d.legacy {
				delete(created, "start")
				status = http.StatusCreated
			}
			response.WriteHeader(status)
			_ = json.NewEncoder(response).Encode(created)
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			listed := map[string]any{
				"id": startSessionID, "cmd": "claude", "tool": "claude-code", "kind": "claude-structured",
				"messageSubmit": true, "start": d.start,
			}
			for key, value := range d.listed {
				listed[key] = value
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{listed}})
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/events"):
			_ = json.NewEncoder(response).Encode(map[string]any{"events": []any{}, "nextIndex": 0})
		case request.Method == http.MethodPost && request.URL.Path == "/api/sessions/"+startSessionID+"/submit":
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			d.submitOps = append(d.submitOps, body["operation_id"])
			receipt := map[string]any{"operation_id": body["operation_id"], "session_id": startSessionID}
			for key, value := range d.submitReceipt {
				receipt[key] = value
			}
			_ = json.NewEncoder(response).Encode(receipt)
		default:
			http.NotFound(response, request)
		}
	}))
}

func runStartCLI(t *testing.T, server *httptest.Server, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SESSIONS_SESSION_ID", "")
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"--host", server.URL}, args...), strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// The session exists but its first request was refused. The answer must carry
// the session identity, the start receipt, and the safe retry, never just an
// error string.
func TestNewReturnsSessionIdentityWhenFirstRequestIsRefused(t *testing.T) {
	daemon := &startDaemon{
		submitReceipt: map[string]any{"status": "not-delivered", "delivered": false, "retry": true, "reason": "provider was not ready"},
		start: map[string]any{"phase": "prompt-not-delivered", "evidence": "the first request was refused", "evidence_source": "delivery-receipt",
			"prompt":   map[string]any{"status": "not-delivered", "retry": true},
			"recovery": map[string]any{"action": "send-prompt", "detail": "Nothing reached the provider."}},
	}
	server := daemon.serve(t)
	defer server.Close()
	code, stdout, stderr := runStartCLI(t, server, "--json", "new", "--tool", "claude", "--structured", "fix the flaky test")
	if code != exitTransport {
		t.Fatalf("exit = %d, want %d; stdout=%s stderr=%s", code, exitTransport, stdout, stderr)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	createOp, _ := daemon.createBodies[0]["operation_id"].(string)
	if result["id"] != startSessionID || result["ok"] != false || result["code"] != float64(exitTransport) {
		t.Fatalf("result identity = %#v", result)
	}
	first, _ := result["first_request"].(map[string]any)
	if first["retry"] != true || first["operation_id"] != daemon.promptOp {
		t.Fatalf("first_request = %#v, want retry:true under %s", first, daemon.promptOp)
	}
	if start, _ := result["start"].(map[string]any); start["phase"] != "prompt-not-delivered" {
		t.Fatalf("start = %#v", result["start"])
	}
	if rerun, _ := result["rerun"].(string); createOp == "" || !strings.Contains(rerun, createOp) {
		t.Fatalf("rerun = %q, want the create operation id %q", result["rerun"], createOp)
	}
}

// Re-running with the reported --operation-id returns the same session and
// delivers the first request under the id the daemon recorded, so the retry
// cannot create a second session or send the request twice.
func TestNewRerunWithOperationIDReusesSessionAndFirstRequestID(t *testing.T) {
	daemon := &startDaemon{
		submitReceipt: map[string]any{"status": "accepted", "delivered": true, "acceptance": "runner"},
		start:         map[string]any{"phase": "prompt-delivered", "evidence": "accepted", "evidence_source": "delivery-receipt"},
	}
	server := daemon.serve(t)
	defer server.Close()
	const operationID = "dddddddd-eeee-4fff-8000-000000000001"
	for attempt := 0; attempt < 2; attempt++ {
		code, stdout, stderr := runStartCLI(t, server, "new", "--tool", "claude", "--structured", "--operation-id", operationID, "fix the flaky test")
		if code != 0 || strings.TrimSpace(stdout) != startSessionID {
			t.Fatalf("attempt %d exit=%d stdout=%q stderr=%q", attempt, code, stdout, stderr)
		}
	}
	if len(daemon.createBodies) != 2 || daemon.createBodies[1]["operation_id"] != operationID {
		t.Fatalf("create bodies = %#v", daemon.createBodies)
	}
	if len(daemon.submitOps) != 2 || daemon.submitOps[0] != daemon.promptOp || daemon.submitOps[1] != daemon.promptOp {
		t.Fatalf("first request operations = %#v, want both %s", daemon.submitOps, daemon.promptOp)
	}
	if code, _, stderr := runStartCLI(t, server, "new", "--tool", "claude", "--operation-id", "NOT-A-UUID"); code != exitUsage || !strings.Contains(stderr, "--operation-id") {
		t.Fatalf("malformed --operation-id: exit %d stderr %q", code, stderr)
	}
}

// An ambiguous delivery keeps the session identity on stdout and tells the
// person not to resend, with the rerun that cannot duplicate the request.
func TestNewAmbiguousFirstRequestExplainsWithoutResending(t *testing.T) {
	daemon := &startDaemon{
		submitReceipt: map[string]any{"status": "unknown", "delivered": false, "retry": false, "reason": "request context deadline exceeded"},
		start: map[string]any{"phase": "prompt-unknown", "evidence": "Sessions cannot prove whether the first request arrived", "evidence_source": "delivery-receipt",
			"recovery": map[string]any{"action": "inspect", "command": "sessions last " + startSessionID + " --role user", "detail": "Sessions will not resend the first request."}},
	}
	server := daemon.serve(t)
	defer server.Close()
	code, stdout, stderr := runStartCLI(t, server, "new", "--tool", "claude", "--structured", "fix the flaky test")
	if code != exitTransport || strings.TrimSpace(stdout) != startSessionID {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, fragment := range []string{"deadline exceeded", "prompt-unknown", "will not resend", "sessions last", "--operation-id"} {
		if !strings.Contains(stderr, fragment) {
			t.Errorf("stderr lacks %q:\n%s", fragment, stderr)
		}
	}
	if len(daemon.submitOps) != 1 {
		t.Fatalf("first request submitted %d times, want once", len(daemon.submitOps))
	}
}

// A login screen that appears before any turn is an authentication failure,
// whatever idle reason the terminal classifier recorded. It was answered as a
// successful idle, so a delegating agent read a signed-out child as finished.
func TestWaitReportsAuthenticationEvenWithoutAFailedTurn(t *testing.T) {
	id := "23000000-0000-4000-8000-000000000031"
	body := `{"sessions":[{"id":"` + id + `","cmd":"claude","cwd":"/tmp","createdAt":1,"pid":1,` +
		`"tool":"claude-code","working":false,"idleReason":"needs-input","failureKind":"auth","failureDetail":"Claude is not signed in",` +
		`"start":{"phase":"blocked","blocked_by":"auth","evidence":"Claude is not signed in","evidence_source":"provider-fault",` +
		`"recovery":{"action":"connect-account","detail":"Connect the Claude account in Sessions → Accounts"}}}]}`
	server := waitTestServer(t, body)
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "--json", "wait", id, "--timeout", "1h"}, strings.NewReader(""), &stdout, &stderr)
	outcome := decodeWaitOutcome(t, stdout.String())
	if code != exitTargetUnavailable || outcome.OK || outcome.Reason != "auth" || outcome.Start == nil || outcome.Start.Recovery.Action != "connect-account" {
		t.Fatalf("auth wait exit=%d outcome=%+v stderr=%q", code, outcome, stderr.String())
	}
}

// An idle session whose first request never provably arrived has not finished
// anything; waiting reports that instead of a successful idle.
func TestWaitReportsUndeliveredFirstRequestInsteadOfIdle(t *testing.T) {
	for _, test := range []struct{ phase, status, reason string }{
		{"prompt-unknown", "unknown", waitReasonPromptUnknown},
		{"prompt-not-delivered", "not-delivered", waitReasonPromptNotDelivered},
		{"created", "not-sent", waitReasonPromptNotSent},
	} {
		t.Run(test.phase, func(t *testing.T) {
			id := "23000000-0000-4000-8000-000000000032"
			body := `{"sessions":[{"id":"` + id + `","cmd":"claude","cwd":"/tmp","createdAt":1,"pid":1,"tool":"claude-code","working":false,` +
				`"start":{"phase":"` + test.phase + `","prompt":{"status":"` + test.status + `","retry":false},"evidence":"first request evidence","evidence_source":"delivery-receipt"}}]}`
			server := waitTestServer(t, body)
			t.Setenv("HOME", t.TempDir())
			var stdout, stderr bytes.Buffer
			code := run([]string{"--host", server.URL, "--json", "wait", id, "--timeout", "1h"}, strings.NewReader(""), &stdout, &stderr)
			outcome := decodeWaitOutcome(t, stdout.String())
			if code != exitTargetUnavailable || outcome.OK || outcome.Reason != test.reason || outcome.Detail != "first request evidence" {
				t.Fatalf("wait exit=%d outcome=%+v stderr=%q", code, outcome, stderr.String())
			}
		})
	}
}

// The status card leads with a blocked start instead of "idle"; the runtime
// word stays beside it because liveness and the task are separate facts.
func TestStatusHeadlineLeadsWithBlockedAuthentication(t *testing.T) {
	if got := statusHeadline("idle", session{FailureKind: "auth"}); got != "BLOCKED (auth) — runtime idle" {
		t.Fatalf("auth headline = %q", got)
	}
	if got := statusHeadline("idle", session{}); got != "idle" {
		t.Fatalf("plain headline = %q", got)
	}
	if got := statusHeadline("exited", session{FailureKind: "auth", Exited: true}); got != "exited" {
		t.Fatalf("exited headline = %q", got)
	}
}

// A first request that is in flight, or accepted with no provider turn after
// it, has not started any work. Waiting must not report that as a finished
// idle session, even when an earlier turn left idleReason completed.
func TestWaitKeepsWaitingUntilDelegatedWorkBegins(t *testing.T) {
	for _, test := range []struct{ name, phase, status string }{
		{"accepted without a provider turn", "prompt-delivered", "accepted"},
		{"delivery still in flight", "created", "sending"},
	} {
		t.Run(test.name, func(t *testing.T) {
			id := "23000000-0000-4000-8000-000000000033"
			body := `{"sessions":[{"id":"` + id + `","cmd":"claude","cwd":"/tmp","createdAt":1,"pid":1,"tool":"claude-code","working":false,` +
				`"idleReason":"completed","idleSince":2,"lastSummary":"an earlier turn",` +
				`"start":{"phase":"` + test.phase + `","prompt":{"status":"` + test.status + `","retry":false,"at":5},"evidence":"e","evidence_source":"delivery-receipt"}}]}`
			server := waitTestServer(t, body)
			t.Setenv("HOME", t.TempDir())
			var stdout, stderr bytes.Buffer
			code := run([]string{"--host", server.URL, "--json", "wait", id, "--idle", "10ms", "--timeout", "1500ms"}, strings.NewReader(""), &stdout, &stderr)
			outcome := decodeWaitOutcome(t, stdout.String())
			if code != exitWaitTimeout || outcome.OK || outcome.Reason != waitReasonTimeout {
				t.Fatalf("wait exit=%d outcome=%+v stderr=%q, want a timeout rather than a successful idle", code, outcome, stderr.String())
			}
		})
	}
}

// An older daemon ignores operation_id. Sessions must not promise that a
// re-run is safe against it, because that re-run would start a second session.
func TestNewDoesNotPromiseIdempotencyToADaemonThatIgnoredTheOperationID(t *testing.T) {
	daemon := &startDaemon{legacy: true,
		submitReceipt: map[string]any{"status": "not-delivered", "delivered": false, "retry": true, "reason": "provider was not ready"}}
	server := daemon.serve(t)
	defer server.Close()
	code, stdout, stderr := runStartCLI(t, server, "--json", "new", "--tool", "claude", "--structured", "fix the flaky test")
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil || code != exitTransport {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	rerun, _ := result["rerun"].(string)
	if strings.Contains(rerun, "returns session") || !strings.Contains(rerun, "second session") || !strings.Contains(rerun, "sessions send "+startSessionID) {
		t.Fatalf("rerun advice to a legacy daemon = %q", rerun)
	}
	if result["start_error"] == nil {
		t.Fatalf("missing start receipt was not reported: %#v", result)
	}
}

// Every create that does not return a usable session answers with a
// structured receipt carrying the operation id, and the session id whenever
// the daemon knows one.
func TestNewCreateFailuresAreStructuredReceipts(t *testing.T) {
	cases := []struct {
		name, outcome, session string
		answer                 func(http.ResponseWriter)
	}{
		{name: "launch failed after the session was recorded", outcome: createOutcomeNotStarted, session: startSessionID, answer: func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "runner did not create socket within 60s", "session_id": startSessionID})
		}},
		{name: "operation already created an ended session", outcome: createOutcomeAlreadyCreated, session: startSessionID, answer: func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "operation already created session", "session_id": startSessionID})
		}},
		{name: "refused before anything was created", outcome: createOutcomeRefused, answer: func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "cwd is not a directory"})
		}},
		{name: "answer lost in transit", outcome: createOutcomeUnknown, answer: func(w http.ResponseWriter) {
			hijacked, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = hijacked.Close()
			}
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost && r.URL.Path == "/api/sessions" {
					test.answer(w)
					return
				}
				http.NotFound(w, r)
			}))
			defer server.Close()
			const operationID = "dddddddd-eeee-4fff-8000-000000000002"
			code, stdout, stderr := runStartCLI(t, server, "--json", "new", "--tool", "claude", "--structured", "--operation-id", operationID, "fix the flaky test")
			var failure createFailure
			if err := json.Unmarshal([]byte(stdout), &failure); err != nil {
				t.Fatalf("stdout is not a receipt: %v\n%s\n%s", err, stdout, stderr)
			}
			if code != exitTransport || failure.OK || failure.Code != exitTransport || failure.Outcome != test.outcome ||
				failure.OperationID != operationID || failure.SessionID != test.session || failure.Next == "" {
				t.Fatalf("exit=%d receipt=%+v", code, failure)
			}
			if test.outcome == createOutcomeUnknown && !strings.Contains(failure.Next, "update it first") {
				t.Fatalf("unknown outcome promised a safe re-run: %q", failure.Next)
			}
		})
	}
}

// The create answer is taken before the first request; the start receipt
// after it. Printing the first's working:false/never-started beside the
// second's phase:working read as a contradiction, so the record must be the
// later read as a whole. A re-run must still say it was a replay.
func TestNewJSONRecordAndStartReceiptAreOneSnapshot(t *testing.T) {
	daemon := &startDaemon{
		submitReceipt: map[string]any{"status": "accepted", "delivered": true, "acceptance": "runner"},
		start:         map[string]any{"phase": "working", "evidence": "the provider reported an active turn", "evidence_source": "provider-events"},
		listed:        map[string]any{"working": true},
	}
	server := daemon.serve(t)
	defer server.Close()
	const operationID = "dddddddd-eeee-4fff-8000-000000000002"
	for attempt := 0; attempt < 2; attempt++ {
		code, stdout, stderr := runStartCLI(t, server, "--json", "new", "--tool", "claude", "--structured", "--operation-id", operationID, "start now")
		var record map[string]any
		if err := json.Unmarshal([]byte(stdout), &record); err != nil || code != 0 {
			t.Fatalf("attempt %d exit=%d stdout=%q stderr=%q err=%v", attempt, code, stdout, stderr, err)
		}
		start, _ := record["start"].(map[string]any)
		if record["working"] != true || record["idleReason"] != nil || start["phase"] != "working" {
			t.Fatalf("attempt %d mixed snapshots: working=%v idleReason=%v phase=%v", attempt, record["working"], record["idleReason"], start["phase"])
		}
		if replayed, _ := start["replayed"].(bool); replayed != (attempt == 1) {
			t.Fatalf("attempt %d start.replayed=%v", attempt, start["replayed"])
		}
		if record["ok"] != true || record["first_request"] == nil {
			t.Fatalf("attempt %d lost the additive fields: %v", attempt, record)
		}
	}
}
