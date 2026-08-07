package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// `sessions rename <session> --auto` asks for no name at all; it gives the
// card back to the provider's own conversation title. Sending a name with it
// would be two contradictory instructions, so the CLI refuses that locally
// rather than letting the daemon guess which one was meant.
func TestRenameAutoAsksTheDaemonToFollowTheProviderTitle(t *testing.T) {
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	var received renameResponse
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []map[string]any{{"id": id, "name": "Texas billing"}}})
		case request.Method == http.MethodPut && request.URL.Path == "/api/sessions/"+id+"/name":
			if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
				http.Error(response, err.Error(), http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"name": "TexasT"})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "rename", id[:8], "--auto"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("rename --auto exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !received.Auto || received.Name != "" {
		t.Fatalf("request = %#v, want auto with no name of its own", received)
	}
	if stdout.String() != "TexasT\n" {
		t.Fatalf("output = %q, want the provider title the daemon adopted", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--host", server.URL, "rename", id[:8], "--auto", "Texas"}, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Fatalf("rename --auto with a name exit=0 stdout=%q, want a refusal", stdout.String())
	}
	if !strings.Contains(stderr.String(), "--auto") {
		t.Fatalf("refusal = %q, want it to name the conflicting flag", stderr.String())
	}
}
