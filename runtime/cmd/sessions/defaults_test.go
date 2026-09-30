package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestDefaultsReadsAndUpdatesClaudePermissionMode(t *testing.T) {
	settings := state.ClaudeSettings{
		RemoteControl: state.ClaudeChoiceOn, PermissionMode: state.ClaudePermissionAuto,
		Model: "opus", Effort: "high", Chrome: state.ClaudeChoiceOff,
		SomewhereMCP: state.ClaudeSomewhereEnsure, RemoteControlNamePrefix: "Mini",
	}
	putCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/claude/settings" {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case http.MethodGet:
			_ = json.NewEncoder(response).Encode(settings)
		case http.MethodPut:
			putCount++
			if err := json.NewDecoder(request.Body).Decode(&settings); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(response).Encode(settings)
		default:
			t.Fatalf("method = %s", request.Method)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "defaults", "--permissions", "full"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if putCount != 1 || settings.PermissionMode != state.ClaudePermissionBypass {
		t.Fatalf("put=%d settings=%#v", putCount, settings)
	}
	if settings.Model != "opus" || settings.RemoteControl != state.ClaudeChoiceOn || settings.SomewhereMCP != state.ClaudeSomewhereEnsure {
		t.Fatalf("unrelated defaults changed: %#v", settings)
	}
	if !strings.Contains(stdout.String(), "Full access (skip permissions)") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestDefaultsRejectsUnknownPermissionWithoutWriting(t *testing.T) {
	putCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodPut {
			putCount++
		}
		_ = json.NewEncoder(response).Encode(state.DefaultClaudeSettings())
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "defaults", "--permissions", "unsafe"}, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if putCount != 0 || !strings.Contains(stderr.String(), "must be settings") {
		t.Fatalf("put=%d stderr=%q", putCount, stderr.String())
	}
}

func TestKeysSendsProviderNativeShiftTab(t *testing.T) {
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	received := ""
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{map[string]any{"id": id, "name": "PM"}}})
		case request.Method == http.MethodPost && request.URL.Path == "/api/sessions/"+id+"/input":
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			received = body["data"]
			_ = json.NewEncoder(response).Encode(map[string]bool{"ok": true})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "keys", id, "shift-tab"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if received != "\x1b[Z" {
		t.Fatalf("received = %q, want shift-tab", received)
	}
}

func TestResumeFullPermissionsRequestsClaudeBypass(t *testing.T) {
	const provider = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	const historyID = "provider-history:claude:" + provider
	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/recovery/adopt" {
			http.NotFound(response, request)
			return
		}
		if err := json.NewDecoder(request.Body).Decode(&posted); err != nil {
			t.Fatal(err)
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusCreated)
		_, _ = response.Write([]byte(`{"ok":true,"laneId":"continued-lane","adoption":{}}`))
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "resume", historyID, "--permissions", "full", "--force"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if posted["claudePermissionMode"] != state.ClaudePermissionBypass {
		t.Fatalf("posted = %#v", posted)
	}
}
