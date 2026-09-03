package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRetryCommandUsesProviderRetryRoutes(t *testing.T) {
	id := "24000000-0000-4000-8000-000000000001"
	for _, test := range []struct {
		args     []string
		path     string
		contains string
	}{
		{args: []string{"retry", id}, path: "/api/sessions/" + id + "/retry", contains: "retry started"},
		{args: []string{"retry", id, "--stop"}, path: "/api/sessions/" + id + "/retry/stop", contains: "automatic retry stopped"},
	} {
		t.Run(test.path, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet && request.URL.Path == "/api/sessions" {
					response.Header().Set("Content-Type", "application/json")
					_, _ = response.Write([]byte(`{"sessions":[{"id":"` + id + `","cmd":"codex","cwd":"/tmp","tool":"codex"}]}`))
					return
				}
				if request.Method != http.MethodPost || request.URL.Path != test.path {
					t.Errorf("request = %s %s", request.Method, request.URL.Path)
				}
				if strings.HasSuffix(test.path, "/stop") {
					response.WriteHeader(http.StatusNoContent)
					return
				}
				response.Header().Set("Content-Type", "application/json")
				_, _ = response.Write([]byte(`{"id":"` + id + `"}`))
			}))
			defer server.Close()
			t.Setenv("HOME", t.TempDir())
			var stdout, stderr bytes.Buffer
			args := append([]string{"--host", server.URL}, test.args...)
			if code := run(args, strings.NewReader(""), &stdout, &stderr); code != 0 ||
				!strings.Contains(stdout.String(), test.contains) {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}
