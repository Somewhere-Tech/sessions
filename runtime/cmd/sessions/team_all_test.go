package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTeamAllRollsLanesUpUnderTheirManagers(t *testing.T) {
	grouped := "manager-2"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path != "/api/sessions" {
			http.NotFound(response, request)
			return
		}
		_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []map[string]any{
			{"id": "manager-1", "name": "Sessions", "tool": "claude", "cwd": "/w/sessions", "working": false, "idleReason": "completed"},
			{"id": "lane-1a", "name": "tests", "tool": "codex", "cwd": "/w/sessions", "working": true, "parent_session_id": "manager-1"},
			{"id": "lane-1b", "name": "docs", "tool": "claude", "cwd": "/w/sessions", "working": false, "idleReason": "needs-input", "idleDetail": "Allow? Run `npm test`", "parent_session_id": "manager-1"},
			{"id": "lane-1c", "name": "grandchild", "tool": "claude", "cwd": "/w/sessions", "working": true, "parent_session_id": "lane-1a"},
			{"id": "manager-2", "name": "Somewhere", "tool": "codex", "cwd": "/w/somewhere", "working": true},
			{"id": "lane-2a", "name": "api", "tool": "codex", "cwd": "/w/somewhere", "working": false, "idleReason": "completed", "display_parent_session_id": grouped},
			{"id": "solo", "name": "notes", "tool": "terminal", "cwd": "/w/notes", "working": false},
			{"id": "ended", "name": "old", "tool": "claude", "cwd": "/w/sessions", "exited": true, "parent_session_id": "manager-1"},
		}})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "--json", "team", "--all"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var report struct {
		Managers []teamRollup `json:"managers"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Managers) != 2 || report.Managers[0].ID != "manager-1" {
		t.Fatalf("managers = %+v", report.Managers)
	}
	first := report.Managers[0]
	if first.Lanes != 3 || first.Working != 2 || first.NeedsYou != 1 || len(first.Waiting) != 1 || first.Waiting[0].ID != "lane-1b" || !strings.Contains(first.Waiting[0].Line, "npm test") {
		t.Fatalf("manager-1 rollup = %+v", first)
	}
	if second := report.Managers[1]; second.ID != "manager-2" || second.Lanes != 1 || second.NeedsYou != 0 {
		t.Fatalf("manager-2 rollup = %+v", second)
	}

	stdout.Reset()
	if code := run([]string{"--host", server.URL, "team", "--all"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	text := stdout.String()
	if !strings.Contains(text, "Sessions") || !strings.Contains(text, "waiting on you") || !strings.Contains(text, "docs") || strings.Contains(text, "notes") {
		t.Fatalf("human output = %q", text)
	}
}
