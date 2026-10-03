package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConversationReadsRetainCompleteClaudePastedUserInstructions(t *testing.T) {
	const id = "9cd94e86-2222-4333-8444-555555555555"
	text := "\n\n<pasted_content id=\"7ab0\">\n# Start now\n" + strings.Repeat("preserve my entire instructions 🙂\n", 256) + "</pasted_content id=\"7ab0\">\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sessions":
			_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []map[string]any{{"id": id, "tool": "claude", "cwd": "/work"}}})
		case "/api/sessions/" + id + "/events":
			_ = json.NewEncoder(w).Encode(map[string]any{"events": []map[string]any{
				{"type": "user", "message": map[string]any{"role": "user", "content": text}},
				{"type": "assistant", "message": map[string]any{"role": "assistant", "content": "Work started."}},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	for _, command := range [][]string{{"cat", id}, {"transcript", id}, {"last", id, "--role", "user"}} {
		var stdout, stderr bytes.Buffer
		args := append([]string{"--host", server.URL}, command...)
		if code := run(args, strings.NewReader(""), &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), text) {
			t.Fatalf("%v exit=%d stdout bytes=%d stderr=%q; complete paste missing", command, code, stdout.Len(), stderr.String())
		}
	}
}
