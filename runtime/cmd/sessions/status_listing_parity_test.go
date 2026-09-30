package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

const parityID = "cccccccc-dddd-4eee-8fff-000000000000"

// A live structured Claude session as the daemon reports it: working, with a
// last summary, a failure it recovered from, and every state field a caller
// needs before deciding to send.
func parityRecord() map[string]any {
	return map[string]any{
		"id": parityID, "name": "coordination", "description": "the session an agent inspects before sending",
		"kind": "claude-structured", "cmd": "claude", "args": []string{"--structured"},
		"cwd": "/tmp/project", "cols": 300, "rows": 50, "createdAt": 1757000000000, "pid": 4242,
		"runnerProtocol": 5, "messageSubmit": true, "runnerVersion": "1.2.3", "tool": "claude",
		"working": true, "lastDataAt": 1757000009000, "lastUserMessageAt": 1757000005000,
		"lastHumanMessageAt": 1757000005000, "lastAgentMessageAt": 1757000008000,
		"idleReason": "completed", "idleDetail": "", "idleSince": 1757000008500,
		"lastSummary": "Implementation is ready for review.",
		"failureKind": "provider-overloaded", "failureDetail": "the provider returned 529", "failureAt": 1757000004000,
		"model": "opus", "effort": "high",
		"exited": false, "unreachable": false, "runnerGone": false,
		"exitCode": nil, "exitSignal": nil, "exitedAt": nil,
		"conversationId": "conversation-1", "claudeSessionId": "claude-1",
		"permissions": "bypass", "lifecycle": "durable", "pinned": true,
	}
}

func parityDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/api/sessions":
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{parityRecord()}})
		case strings.HasSuffix(request.URL.Path, "/verdict"):
			// What the daemon answers for a session that has never emitted one.
			http.NotFound(response, request)
		default:
			http.NotFound(response, request)
		}
	}))
}

// An agent that inspects one session before sending to it must not learn less
// than one that listed everything. Every field `sessions ls --json` reports for
// a session is in `sessions status --json` for that same session, with the same
// name and the same value.
func TestStatusJSONCarriesEveryListingField(t *testing.T) {
	server := parityDaemon(t)
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var listOut, listErr bytes.Buffer
	if code := run([]string{"--host", server.URL, "--json", "ls"}, strings.NewReader(""), &listOut, &listErr); code != 0 {
		t.Fatalf("ls exit=%d stderr=%q", code, listErr.String())
	}
	var listed []map[string]any
	if err := json.Unmarshal(listOut.Bytes(), &listed); err != nil {
		t.Fatalf("decode ls: %v\n%s", err, listOut.String())
	}
	if len(listed) != 1 {
		t.Fatalf("ls returned %d sessions, want 1", len(listed))
	}

	var statusOut, statusErr bytes.Buffer
	if code := run([]string{"--host", server.URL, "--json", "status", parityID}, strings.NewReader(""), &statusOut, &statusErr); code != 0 {
		t.Fatalf("status exit=%d stderr=%q", code, statusErr.String())
	}
	var described map[string]any
	if err := json.Unmarshal(statusOut.Bytes(), &described); err != nil {
		t.Fatalf("decode status: %v\n%s", err, statusOut.String())
	}

	missing := make([]string, 0)
	for key, want := range listed[0] {
		got, present := described[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		wantJSON, _ := json.Marshal(want)
		gotJSON, _ := json.Marshal(got)
		if string(wantJSON) != string(gotJSON) {
			t.Errorf("status[%q] = %s, ls says %s", key, gotJSON, wantJSON)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("status is missing %v that ls reports\n%s", missing, statusOut.String())
	}

	// The state facts this slice exists for, named rather than counted.
	for key, want := range map[string]any{
		"working": true, "idleReason": "completed", "lastSummary": "Implementation is ready for review.",
		"exited": false, "unreachable": false, "failureKind": "provider-overloaded",
	} {
		encodedGot, _ := json.Marshal(described[key])
		encodedWant, _ := json.Marshal(want)
		if string(encodedGot) != string(encodedWant) {
			t.Errorf("status[%q] = %s, want %s", key, encodedGot, encodedWant)
		}
	}
	// And what status adds on top.
	for _, key := range []string{"record", "state", "git", "age_ms", "created_at", "last_activity_at"} {
		if _, present := described[key]; !present {
			t.Errorf("status lost its own field %q", key)
		}
	}
}

// The card a person reads says the same facts in the same words.
func TestStatusCardStatesTheListingFacts(t *testing.T) {
	server := parityDaemon(t)
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "status", parityID}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("status exit=%d stderr=%q", code, stderr.String())
	}
	card := stdout.String()
	t.Logf("card:\n%s", card)
	for _, line := range []string{
		"working  yes",
		"exited   no",
		"reason   completed",
		"summary  Implementation is ready for review.",
		"failure  provider-overloaded — the provider returned 529",
		"kind     claude-structured",
	} {
		if !strings.Contains(card, line) {
			t.Errorf("status card does not state %q", line)
		}
	}
}
