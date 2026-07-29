package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestArchiveReturnsTheSameFailureStatusForHumanAndJSONOutput(t *testing.T) {
	const sessionID = "00000000-0000-4000-8000-000000000001"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			_, _ = io.WriteString(response, `{"sessions":[{"id":"`+sessionID+`","name":"done","exited":true}]}`)
		case request.Method == http.MethodPost && request.URL.Path == "/api/retention/archive":
			_, _ = io.WriteString(response, `{"dry_run":false,"items":[{"id":"`+sessionID+`","name":"done","status":"skipped","reason":"already archived"}]}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	for _, test := range []struct {
		name string
		args []string
		json bool
	}{
		{name: "human", args: []string{"--host", server.URL, "archive", sessionID}},
		{name: "json", args: []string{"--json", "--host", server.URL, "archive", sessionID}, json: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			stdout, stderr, code := runGCCLI("", test.args...)
			if code != 2 || !strings.Contains(stderr, "no selected records were archived") {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if test.json {
				var result retentionResult
				if err := json.Unmarshal([]byte(stdout), &result); err != nil {
					t.Fatalf("JSON output = %q: %v", stdout, err)
				}
				if len(result.Items) != 1 || result.Items[0].Status != "skipped" {
					t.Fatalf("JSON archive result = %#v", result)
				}
			}
		})
	}
}
