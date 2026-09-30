package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLanesReportsLostRunnerWithItsClosingCommand(t *testing.T) {
	const id = "41000000-0000-4000-8000-000000000001"
	laneJSON := `{"id":"` + id + `","name":"sleeper","kind":"lane","tool":"lane","cwd":"/tmp","exited":false,"unreachable":true,"unreachableReason":"runner-lost","runnerGone":true,"lane_status":{"state":"lost","reason":"runner process is gone","command":"sessions kill ` + id + `"}}`
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/lanes":
			_, _ = response.Write([]byte(`{"lanes":[` + laneJSON + `]}`))
		case "/api/sessions":
			_, _ = response.Write([]byte(`{"sessions":[` + laneJSON + `]}`))
		case "/api/lanes/" + id + "/manifest":
			response.WriteHeader(http.StatusNotFound)
			_, _ = response.Write([]byte(`{"error":"unknown lane"}`))
		case "/api/sessions/" + id:
			if request.Method != http.MethodDelete {
				http.NotFound(response, request)
				return
			}
			_, _ = response.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "lanes"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("lanes exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"lost", "runner process is gone", "sessions kill " + id} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("lost lane table omitted %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	code = run([]string{"--host", server.URL, "--json", "lanes"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("JSON lanes exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var lanes []laneView
	if err := json.Unmarshal(stdout.Bytes(), &lanes); err != nil {
		t.Fatal(err)
	}
	if len(lanes) != 1 || lanes[0].LaneStatus == nil || lanes[0].LaneStatus.State != "lost" ||
		lanes[0].LaneStatus.Command != "sessions kill "+id {
		t.Fatalf("lost lane JSON = %#v", lanes)
	}

	stdout.Reset()
	code = run([]string{"--host", server.URL, "list", "-a"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "lost") ||
		!strings.Contains(stdout.String(), "sessions kill "+id) {
		t.Fatalf("combined lost listing exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	code = run([]string{"--host", server.URL, "ls", "-a"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "(no sessions)") {
		t.Fatalf("ls must keep excluding headless lanes: exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	code = run([]string{"--host", server.URL, "kill", id}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "closed lost record "+id) {
		t.Fatalf("close lost record exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestSharedListStateAndRecoveryDoNotCallLostLanesRunning(t *testing.T) {
	const id = "41000000-0000-4000-8000-000000000002"
	lost := session{
		ID: id, Kind: "lane", Tool: "lane", Unreachable: true,
		UnreachableReason: "runner-lost", RunnerGone: true,
	}
	if got := sessionState(lost); got != "lost" {
		t.Fatalf("sessionState(lost lane) = %q, want lost", got)
	}
	if got := sessionRecoveryCommand(lost); got != "sessions kill "+id {
		t.Fatalf("lost lane recovery = %q", got)
	}

	conversation := lost
	conversation.Kind = "codex-app-server"
	conversation.Tool = "codex"
	conversation.ConversationID = "42000000-0000-4000-8000-000000000001"
	if got := sessionRecoveryCommand(conversation); got != "sessions resume "+id {
		t.Fatalf("lost conversation recovery = %q", got)
	}
}
