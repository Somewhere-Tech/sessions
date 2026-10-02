package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRestartCLIRequiresExactConfirmationAndReportsPartialJSON(t *testing.T) {
	const source = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	var posted map[string]any
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/recovery/restart" || r.Method != http.MethodPost {
			t.Errorf("unexpected route %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&posted)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(202)
		w.Write([]byte(`{"ok":false,"partial":true,"sourceEnded":true,"sourceSessionId":"` + source + `","laneId":"replacement","operationId":"stable","error":"replacement needs repair"}`))
	}))
	defer server.Close()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("SESSIONS_STATE_DIR", root+"/runners")
	t.Setenv("SESSIONS_LEDGER_PATH", root+"/lanes.sqlite3")
	t.Setenv("SESSIONS_PORT", "8899")
	var out, errOut bytes.Buffer
	code := run([]string{"--host", server.URL, "--json", "restart", source, "--permissions", "full"}, strings.NewReader(""), &out, &errOut)
	if code == 0 || calls != 0 {
		t.Fatalf("missing confirmation exit=%d calls=%d", code, calls)
	}
	out.Reset()
	errOut.Reset()
	code = run([]string{"--host", server.URL, "--json", "restart", source, "--confirm", source, "--permissions", "full", "--remote-control", "--terminal"}, strings.NewReader(""), &out, &errOut)
	if code != 2 || calls != 1 {
		t.Fatalf("partial exit=%d calls=%d out=%s err=%s", code, calls, out.String(), errOut.String())
	}
	if posted["sourceSessionId"] != source || posted["confirmSessionId"] != source || posted["permissions"] != "full" || posted["remoteControl"] != true || posted["runtimeMode"] != "terminal" {
		t.Fatalf("posted=%+v", posted)
	}
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("JSON output corrupted: %s", out.String())
	}
	if result["laneId"] != "replacement" || result["sourceEnded"] != true || result["operationId"] != "stable" {
		t.Fatalf("result=%+v", result)
	}
}

func TestRestartPreviewCLIIsReadOnlyAndNeedsNoConfirmation(t *testing.T) {
	const source = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	previews := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/sessions":
			w.Write([]byte(`{"sessions":[{"id":"` + source + `","name":"New PM"}]}`))
		case "/api/recovery/restart/preview":
			previews++
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if r.Method != "POST" || body["sourceSessionId"] != source {
				t.Errorf("preview request=%s %+v", r.Method, body)
			}
			w.Write([]byte(`{"sourceSessionId":"` + source + `","accountChanged":true,"warning":"changed saved login"}`))
		default:
			t.Errorf("preview attempted unexpected route %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("SESSIONS_STATE_DIR", root+"/runners")
	t.Setenv("SESSIONS_LEDGER_PATH", root+"/lanes.sqlite3")
	t.Setenv("SESSIONS_PORT", "8899")
	var out, errOut bytes.Buffer
	code := run([]string{"--host", server.URL, "--json", "restart", source, "--preview"}, strings.NewReader(""), &out, &errOut)
	if code != 0 || previews != 1 || !strings.Contains(out.String(), `"accountChanged": true`) {
		t.Fatalf("code=%d previews=%d out=%s err=%s", code, previews, out.String(), errOut.String())
	}
}
