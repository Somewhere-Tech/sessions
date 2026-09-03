package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// waitTestServer answers /api/sessions with the supplied session list body.
func waitTestServer(t *testing.T, sessionsBody string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path != "/api/sessions" {
			http.NotFound(response, request)
			return
		}
		_, _ = response.Write([]byte(sessionsBody))
	}))
	t.Cleanup(server.Close)
	return server
}

func decodeWaitOutcome(t *testing.T, raw string) waitOutcome {
	t.Helper()
	var outcome waitOutcome
	if err := json.Unmarshal([]byte(raw), &outcome); err != nil {
		t.Fatalf("wait did not emit a decodable envelope: %v (raw %q)", err, raw)
	}
	return outcome
}

// A delegating agent waits on work it did not launch itself. If the target is
// gone, that has to be legible as failure: this branch used to answer ok:true
// with exit 0, so every loop written as `if rc == 0` treated a dead delegate
// as a finished one.
func TestWaitReportsAVanishedTargetAsFailure(t *testing.T) {
	id := "23000000-0000-4000-8000-000000000009"
	// The target has to exist long enough to be resolved and then disappear,
	// which is exactly what a delegate that dies mid-task does.
	live := `{"sessions":[{"id":"` + id + `","cmd":"codex","cwd":"/tmp","createdAt":1,"pid":1,` +
		`"tool":"codex","working":true}]}`
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path != "/api/sessions" {
			http.NotFound(response, request)
			return
		}
		calls++
		if calls == 1 {
			_, _ = response.Write([]byte(live))
			return
		}
		_, _ = response.Write([]byte(`{"sessions":[]}`))
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"--host", server.URL, "--json", "wait", id, "--timeout", "1h"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if code != exitTargetUnavailable {
		t.Fatalf("gone exit=%d, want %d — waiting longer cannot help", code, exitTargetUnavailable)
	}
	outcome := decodeWaitOutcome(t, stdout.String())
	if outcome.OK {
		t.Fatal("gone reported ok:true; a vanished delegate is not a completed one")
	}
	if outcome.Reason != waitReasonGone {
		t.Fatalf("reason = %q, want %q", outcome.Reason, waitReasonGone)
	}
	if outcome.Session != id {
		t.Fatalf("session = %q, want the requested target %q", outcome.Session, id)
	}
}

// Every branch answers with the same keys, and --summary changes only the
// prose. It used to decide whether the caller learned which session answered,
// so the schema depended on a display flag.
func TestWaitEnvelopeIsIdenticalWithAndWithoutSummary(t *testing.T) {
	id := "23000000-0000-4000-8000-000000000010"
	body := `{"sessions":[{"id":"` + id + `","cmd":"codex","cwd":"/tmp","createdAt":1,"pid":1,` +
		`"tool":"codex","working":false,"lastSummary":"Finished the sweep."}]}`
	server := waitTestServer(t, body)
	t.Setenv("HOME", t.TempDir())

	keysFor := func(args ...string) []string {
		var stdout, stderr bytes.Buffer
		base := []string{"--host", server.URL, "--json", "wait", id, "--idle", "0s", "--timeout", "1h"}
		if code := run(append(base, args...), strings.NewReader(""), &stdout, &stderr); code != exitSatisfied {
			t.Fatalf("idle wait exit=%d stderr=%q", code, stderr.String())
		}
		var generic map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &generic); err != nil {
			t.Fatalf("undecodable envelope: %v", err)
		}
		keys := make([]string, 0, len(generic))
		for key := range generic {
			if key != "summary" {
				keys = append(keys, key)
			}
		}
		return keys
	}

	plain := keysFor()
	withSummary := keysFor("--summary")
	if len(plain) != len(withSummary) {
		t.Fatalf("--summary changed the envelope shape: %v vs %v", plain, withSummary)
	}
	for _, key := range plain {
		found := false
		for _, other := range withSummary {
			if key == other {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("key %q disappeared when --summary was passed", key)
		}
	}
}

// The identity of the target is what a fan-out caller needs most, so it is
// present even when the wait did not succeed.
func TestWaitTimeoutNamesItsTargetAndIsNotATransportError(t *testing.T) {
	id := "23000000-0000-4000-8000-000000000011"
	body := `{"sessions":[{"id":"` + id + `","cmd":"codex","cwd":"/tmp","createdAt":1,"pid":1,` +
		`"tool":"codex","working":true}]}`
	server := waitTestServer(t, body)
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"--host", server.URL, "--json", "wait", id, "--idle", "1h", "--timeout", "0s"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if code != exitWaitTimeout {
		t.Fatalf("timeout exit=%d, want %d distinct from transport failure %d",
			code, exitWaitTimeout, exitTransport)
	}
	outcome := decodeWaitOutcome(t, stdout.String())
	if outcome.OK || outcome.Reason != waitReasonTimeout {
		t.Fatalf("outcome = %+v, want ok:false reason:timeout", outcome)
	}
	if outcome.Session != id {
		t.Fatalf("session = %q, want %q — a fan-out caller cannot tell who timed out otherwise",
			outcome.Session, id)
	}
}

// needs-input is actionable, so it stays a success; failed is not.
func TestWaitSeparatesActionableStopsFromFailure(t *testing.T) {
	for _, testCase := range []struct {
		idleReason string
		wantReason string
		wantOK     bool
		wantCode   int
	}{
		{"needs-input", waitReasonNeedsInput, true, exitSatisfied},
		{"failed", waitReasonFailed, false, exitTargetUnavailable},
	} {
		t.Run(testCase.idleReason, func(t *testing.T) {
			id := "23000000-0000-4000-8000-000000000012"
			body := `{"sessions":[{"id":"` + id + `","cmd":"codex","cwd":"/tmp","createdAt":1,"pid":1,` +
				`"tool":"codex","working":false,"idleReason":"` + testCase.idleReason + `"}]}`
			server := waitTestServer(t, body)
			t.Setenv("HOME", t.TempDir())

			var stdout, stderr bytes.Buffer
			code := run(
				[]string{"--host", server.URL, "--json", "wait", id, "--timeout", "1h"},
				strings.NewReader(""), &stdout, &stderr,
			)
			if code != testCase.wantCode {
				t.Fatalf("%s exit=%d, want %d", testCase.idleReason, code, testCase.wantCode)
			}
			outcome := decodeWaitOutcome(t, stdout.String())
			if outcome.OK != testCase.wantOK || outcome.Reason != testCase.wantReason {
				t.Fatalf("outcome = %+v, want ok:%v reason:%q",
					outcome, testCase.wantOK, testCase.wantReason)
			}
		})
	}
}

func TestWaitCarriesProviderFailureKindAsReason(t *testing.T) {
	id := "23000000-0000-4000-8000-000000000013"
	body := `{"sessions":[{"id":"` + id + `","cmd":"codex","cwd":"/tmp","createdAt":1,"pid":1,` +
		`"tool":"codex","working":false,"idleReason":"failed","idleDetail":"Codex API unavailable (503, overloaded)",` +
		`"lastSummary":"Codex API unavailable (503, overloaded)","failureKind":"provider-unavailable"}]}`
	server := waitTestServer(t, body)
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "--json", "wait", id, "--timeout", "1h"}, strings.NewReader(""), &stdout, &stderr)
	outcome := decodeWaitOutcome(t, stdout.String())
	if code != exitTargetUnavailable || outcome.OK || outcome.Reason != "provider-unavailable" ||
		outcome.Detail != "Codex API unavailable (503, overloaded)" {
		t.Fatalf("provider wait exit=%d outcome=%+v stderr=%q", code, outcome, stderr.String())
	}
}

func TestWaitTreatsScheduledRetryAsWorking(t *testing.T) {
	id := "23000000-0000-4000-8000-000000000014"
	body := `{"sessions":[{"id":"` + id + `","cmd":"codex","cwd":"/tmp","createdAt":1,"pid":1,` +
		`"tool":"codex","working":false,"idleReason":"failed","failureKind":"provider-unavailable",` +
		`"retry":{"attempt":2,"max":5,"nextAt":9999999999999,"kind":"provider-unavailable"}}]}`
	server := waitTestServer(t, body)
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "--json", "wait", id, "--timeout", "1ms"}, strings.NewReader(""), &stdout, &stderr)
	outcome := decodeWaitOutcome(t, stdout.String())
	if code != exitWaitTimeout || outcome.Reason != waitReasonTimeout || !outcome.Working {
		t.Fatalf("retry wait exit=%d outcome=%+v stderr=%q", code, outcome, stderr.String())
	}
}
