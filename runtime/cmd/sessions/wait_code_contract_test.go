package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// `sessions help` tells an agent that every --json document carries a code
// matching the exit status. The wait envelope did not have one, so an agent
// that followed the instruction and read the key it was promised got nothing,
// and the ordinary way to read a missing number -- treat it as zero -- turned
// a lane that failed with exit 3 into a success. That is the same silent
// wrong-success that ok was introduced to end, arriving through the field the
// documentation pointed at.
//
// These tests assert the invariant rather than the number: whatever the
// process exits with, the document says the same thing. A future branch that
// invents a new status cannot satisfy them by accident.
func decodeGenericDocument(t *testing.T, raw string) map[string]any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal([]byte(raw), &document); err != nil {
		t.Fatalf("undecodable JSON document: %v (raw %q)", err, raw)
	}
	return document
}

// assertCodeMatchesExit is the whole contract in one place.
func assertCodeMatchesExit(t *testing.T, label string, exit int, raw string) {
	t.Helper()
	document := decodeGenericDocument(t, raw)
	reported, present := document["code"]
	if !present {
		t.Fatalf("%s: exited %d but the document has no code field — an agent "+
			"reading the documented key sees a missing value and calls it success; document %q",
			label, exit, raw)
	}
	number, ok := reported.(float64)
	if !ok {
		t.Fatalf("%s: code = %#v, want a number", label, reported)
	}
	if int(number) != exit {
		t.Fatalf("%s: code = %d but the process exited %d — the document and the "+
			"exit status disagree, so branching on either gives a different answer",
			label, int(number), exit)
	}
}

func TestWaitEnvelopeCodeMatchesTheExitStatus(t *testing.T) {
	t.Run("a target that vanished", func(t *testing.T) {
		id := "24000000-0000-4000-8000-000000000001"
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
		exit := run([]string{"--host", server.URL, "--json", "wait", id, "--timeout", "1h"},
			strings.NewReader(""), &stdout, &stderr)
		if exit != exitTargetUnavailable {
			t.Fatalf("exit=%d, want %d; stderr=%q", exit, exitTargetUnavailable, stderr.String())
		}
		assertCodeMatchesExit(t, "gone", exit, stdout.String())
	})

	t.Run("a target that is idle", func(t *testing.T) {
		id := "24000000-0000-4000-8000-000000000002"
		body := `{"sessions":[{"id":"` + id + `","cmd":"codex","cwd":"/tmp","createdAt":1,"pid":1,` +
			`"tool":"codex","working":false}]}`
		server := waitTestServer(t, body)
		t.Setenv("HOME", t.TempDir())

		var stdout, stderr bytes.Buffer
		exit := run([]string{"--host", server.URL, "--json", "wait", id, "--idle", "0s", "--timeout", "1h"},
			strings.NewReader(""), &stdout, &stderr)
		if exit != exitSatisfied {
			t.Fatalf("exit=%d, want %d; stderr=%q", exit, exitSatisfied, stderr.String())
		}
		// Success is the case that matters most: it is the one an omitempty
		// field would erase, leaving the document silent exactly when a
		// caller most needs to distinguish "zero" from "not stated".
		assertCodeMatchesExit(t, "idle", exit, stdout.String())
	})
}

// A lane does not report the status its reason implies -- it propagates the
// child's own exit code -- so this is the path where a code derived from the
// reason instead of from the returned status would be wrong.
func TestLaneWaitCodeIsTheChildExitCode(t *testing.T) {
	const id = "24000000-0000-4000-8000-000000000003"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/lanes":
			_, _ = response.Write([]byte(`{"lanes":[{"id":"` + id + `","kind":"lane","tool":"lane:sh"}]}`))
		case "/api/lanes/" + id + "/manifest":
			_, _ = response.Write([]byte(`{"exit_code":3,"duration_ms":12,"last_output_tail":"boom\n","spec_path":""}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	exit := run([]string{"--host", server.URL, "--json", "wait", id, "--timeout", "5s"},
		strings.NewReader(""), &stdout, &stderr)
	if exit != 3 {
		t.Fatalf("exit=%d, want the child's 3; stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	assertCodeMatchesExit(t, "failed lane", exit, stdout.String())

	outcome := decodeWaitOutcome(t, stdout.String())
	if outcome.Code != 3 || outcome.Lane == nil || outcome.Lane.ExitCode != 3 {
		t.Fatalf("code=%d lane=%+v; the top-level code must be the child's status, "+
			"not the 4 that reason:failed implies", outcome.Code, outcome.Lane)
	}
}

// Every result inside a join carries its own status. Without that, a fan-out
// over five delegates reports code 0 on each one while the aggregate says
// something failed, and the caller cannot tell which delegate to re-run.
func TestJoinCarriesACodePerResultAndForTheJoin(t *testing.T) {
	const good = "24000000-0000-4000-8000-000000000004"
	const bad = "24000000-0000-4000-8000-000000000005"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/lanes":
			_, _ = response.Write([]byte(`{"lanes":[{"id":"` + good + `","kind":"lane","tool":"lane:sh"},` +
				`{"id":"` + bad + `","kind":"lane","tool":"lane:sh"}]}`))
		case "/api/lanes/" + good + "/manifest":
			_, _ = response.Write([]byte(`{"exit_code":0,"duration_ms":10,"last_output_tail":"ok\n","spec_path":""}`))
		case "/api/lanes/" + bad + "/manifest":
			_, _ = response.Write([]byte(`{"exit_code":9,"duration_ms":10,"last_output_tail":"bad\n","spec_path":""}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	exit := run([]string{"--host", server.URL, "--json", "wait", good, bad, "--all", "--timeout", "5s"},
		strings.NewReader(""), &stdout, &stderr)
	if exit == exitSatisfied {
		t.Fatalf("a join containing a failed delegate exited 0; stdout=%q", stdout.String())
	}
	assertCodeMatchesExit(t, "join", exit, stdout.String())

	var join waitJoinOutcome
	if err := json.Unmarshal(stdout.Bytes(), &join); err != nil {
		t.Fatalf("undecodable join: %v (raw %q)", err, stdout.String())
	}
	codes := map[string]int{}
	for _, result := range join.Results {
		codes[result.Session] = result.Code
	}
	if codes[good] != 0 {
		t.Fatalf("the delegate that succeeded reported code %d", codes[good])
	}
	if codes[bad] != 9 {
		t.Fatalf("the delegate that failed reported code %d, want its own exit 9 — "+
			"a caller re-running failures needs to know which one and why", codes[bad])
	}
}

// Dispatching without --wait answers with the lane record, not a wait
// envelope. It is still a --json document, and the promise in `sessions help`
// does not carve out an exception for it.
func TestDispatchWithoutWaitStillCarriesACode(t *testing.T) {
	const id = "24000000-0000-4000-8000-000000000006"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/lanes" && request.Method == http.MethodPost {
			_, _ = response.Write([]byte(`{"id":"` + id + `","kind":"lane","tool":"lane:sh","cmd":"sh"}`))
			return
		}
		http.NotFound(response, request)
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	exit := run([]string{"--host", server.URL, "--json", "run", "--", "sh", "-c", "true"},
		strings.NewReader(""), &stdout, &stderr)
	if exit != exitSatisfied {
		t.Fatalf("exit=%d, want %d; stderr=%q", exit, exitSatisfied, stderr.String())
	}
	assertCodeMatchesExit(t, "dispatch", exit, stdout.String())
	if document := decodeGenericDocument(t, stdout.String()); document["id"] != id {
		t.Fatalf("the lane record lost its id: %v", document["id"])
	}
}
