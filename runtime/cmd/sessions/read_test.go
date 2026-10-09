package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadCLIForwardsCursorAndReportsErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const id = "11111111-2222-4333-8444-555555555555"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/history" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sessions":[{"id":"` + id + `","tool":"claude"}]}`))
			return
		}
		if r.URL.Path != "/api/history/"+id+"/read" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "changed" {
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"code":"HISTORY_CHANGED","error":"history changed"}`))
			return
		}
		if r.URL.Query().Get("cursor") != "opaque-token" || r.URL.Query().Get("limit") != "7" {
			t.Errorf("query=%s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"conversation":"claude:abc","messages":[{"role":"assistant","text":"new answer","timestamp":null}],"next_cursor":"next-token","has_more":false}`))
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "--json", "read", id, "--cursor", "opaque-token", "--limit", "7"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "next-token") {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"--host", server.URL, "--json", "read", id, "--cursor", "changed"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 || !strings.Contains(stdout.String()+stderr.String(), "history changed") {
		t.Fatalf("error code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
}

func TestLastSkipsToolOnlyAssistantRecords(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const id = "11111111-2222-4333-8444-555555555555"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/sessions" {
			_, _ = w.Write([]byte(`{"sessions":[{"id":"` + id + `","tool":"claude-code","cmd":"claude"}]}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"events": []any{
			map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": "real answer"}},
			map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "name": "Read"}}}},
		}})
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "--json", "last", id[:8], "--role", "assistant"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "real answer") {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
}
