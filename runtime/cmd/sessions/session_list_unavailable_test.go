package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A daemon that could not read its durable session record answers 503. The
// CLI must report that as a failed read, never as an empty list or an unknown
// session.
func TestUnavailableSessionStateIsAFailedReadNotAnEmptyList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/sessions" {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusServiceUnavailable)
		_, _ = response.Write([]byte(`{"code":"SESSION_STATE_UNAVAILABLE","action":"retry","error":"Sessions could not read its durable session record. Nothing was changed."}`))
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	// A test run from inside a Sessions session must not inherit its identity.
	t.Setenv("SESSIONS_SESSION_ID", "")
	t.Setenv("SESSIONS_OWNER_ID", "")

	for _, args := range [][]string{
		{"--host", server.URL, "ls", "-a"},
		{"--host", server.URL, "--json", "ls", "-a"},
		{"--host", server.URL, "status", "4d1a7f3e"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(args, strings.NewReader(""), &stdout, &stderr)
		output := stdout.String() + stderr.String()
		if code == 0 {
			t.Fatalf("%q succeeded on an unreadable session record: %q", args, output)
		}
		if !strings.Contains(output, "SESSION_STATE_UNAVAILABLE") && !strings.Contains(output, "durable session record") {
			t.Fatalf("%q did not pass the daemon's reason on: %q", args, output)
		}
		if strings.Contains(output, "unknown session") || strings.Contains(stdout.String(), `"sessions":[]`) {
			t.Fatalf("%q presented the failure as an absence: %q", args, output)
		}
	}
}
