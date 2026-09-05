package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const steerTestSessionID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

func TestSendSteerUsesStructuredMode(t *testing.T) {
	var body map[string]string
	inputCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{map[string]any{
				"id": steerTestSessionID, "cmd": "codex", "tool": "codex", "messageSubmit": true,
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions/"+steerTestSessionID+"/events":
			_ = json.NewEncoder(response).Encode(map[string]any{"events": []any{}, "nextIndex": 0})
		case request.Method == http.MethodPost && request.URL.Path == "/api/sessions/"+steerTestSessionID+"/submit":
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(response).Encode(map[string]any{
				"operation_id": body["operation_id"], "session_id": steerTestSessionID,
				"status": "accepted", "delivered": true, "acceptance": "provider",
			})
		case request.Method == http.MethodPost && request.URL.Path == "/api/sessions/"+steerTestSessionID+"/input":
			inputCalls++
			http.Error(response, "terminal fallback must not run", http.StatusInternalServerError)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "--json", "send", steerTestSessionID, "--steer", "Check the package"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("send --steer exit=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if body["mode"] != "steer" || body["data"] != "Check the package" || body["operation_id"] == "" {
		t.Fatalf("submit body = %#v", body)
	}
	if inputCalls != 0 {
		t.Fatalf("typed acknowledgement retried through /input %d times", inputCalls)
	}
	var result sendJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Submitted == nil || !*result.Submitted || result.Confidence != "provider-accepted" {
		t.Fatalf("send result = %+v", result)
	}
}

func TestSendSteerRejectsUnsupportedSessionBeforeWriting(t *testing.T) {
	writeCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet && request.URL.Path == "/api/sessions" {
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{map[string]any{
				"id": steerTestSessionID, "cmd": "codex", "tool": "codex", "messageSubmit": false,
			}}})
			return
		}
		if request.Method == http.MethodPost {
			writeCalls++
		}
		http.NotFound(response, request)
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "send", steerTestSessionID, "--steer", "Do not type this"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 || writeCalls != 0 {
		t.Fatalf("unsupported steer exit=%d writes=%d stderr=%q", code, writeCalls, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Nothing was sent") {
		t.Fatalf("unsupported steer error = %q", stderr.String())
	}
}

func TestStructuredAcknowledgementNeverRetriesTerminalEnter(t *testing.T) {
	inputCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{map[string]any{
				"id": steerTestSessionID, "cmd": "codex", "tool": "codex", "messageSubmit": true,
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions/"+steerTestSessionID+"/events":
			_ = json.NewEncoder(response).Encode(map[string]any{"events": []any{}, "nextIndex": 0})
		case request.Method == http.MethodPost && request.URL.Path == "/api/sessions/"+steerTestSessionID+"/submit":
			var body map[string]string
			_ = json.NewDecoder(request.Body).Decode(&body)
			_ = json.NewEncoder(response).Encode(map[string]any{
				"operation_id": body["operation_id"], "session_id": steerTestSessionID,
				"status": "accepted", "delivered": true, "acceptance": "runner",
			})
		case request.Method == http.MethodPost && request.URL.Path == "/api/sessions/"+steerTestSessionID+"/input":
			inputCalls++
		case request.URL.Path == "/api/sessions/"+steerTestSessionID+"/snapshot":
			t.Fatal("typed acknowledgement must not enter terminal confirmation polling")
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "--json", "send", steerTestSessionID, "--operation-id", "11111111-2222-4333-8444-555555555555", "Follow up"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || inputCalls != 0 {
		t.Fatalf("structured send exit=%d input calls=%d stderr=%q stdout=%q", code, inputCalls, stderr.String(), stdout.String())
	}
	var result sendJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Confidence != "runner-accepted" {
		t.Fatalf("confidence = %q, want runner-accepted", result.Confidence)
	}
}
