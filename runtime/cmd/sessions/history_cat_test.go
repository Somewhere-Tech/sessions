package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCatAndResurrectUseFleetSearchReference(t *testing.T) {
	const historyID = "provider-history:claude:conversation-one"
	var continued string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/fleet/machine-mini/api/history/"+historyID:
			_, _ = io.WriteString(response, "[user]\nRemember the launch.\n")
		case request.Method == http.MethodPost && request.URL.Path == "/api/fleet/machine-mini/api/recovery/adopt":
			var body map[string]any
			_ = json.NewDecoder(request.Body).Decode(&body)
			continued, _ = body["historyId"].(string)
			_, _ = io.WriteString(response, `{"ok":true,"laneId":"continued-lane","adoption":{}}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SESSIONS_HOST", server.URL)
	if _, err := saveMachine(home, savedMachine{
		Alias: "mini", MachineID: "machine-mini", Name: "Mini", Endpoint: server.URL,
	}, "device-secret"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"cat", "mini::" + historyID}, strings.NewReader(""), &stdout, &stderr); code != 0 || stderr.Len() != 0 || stdout.String() != "[user]\nRemember the launch.\n" {
		t.Fatalf("cat exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"resurrect", "mini::" + historyID}, strings.NewReader(""), &stdout, &stderr); code != 0 || stdout.String() != "continued-lane\n" {
		t.Fatalf("resurrect exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if continued != historyID {
		t.Fatalf("continued history = %q", continued)
	}
}

func TestCatAcceptsLiveSessionsIDLikeTranscript(t *testing.T) {
	const id = "9cd94e86-2222-4333-8444-555555555555"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []map[string]any{{
				"id": id, "name": "review", "tool": "codex", "cwd": "/work",
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions/"+id+"/events":
			_ = json.NewEncoder(response).Encode(map[string]any{"events": []map[string]any{{
				"type": "user", "message": map[string]any{"role": "user", "content": "Review this."},
			}}})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "cat", id[:8]}, strings.NewReader(""), &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "Review this.") {
		t.Fatalf("cat exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestCatPrintsProviderFaultAsError(t *testing.T) {
	const id = "9cd94e86-2222-4333-8444-555555555556"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/sessions":
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []map[string]any{{
				"id": id, "tool": "codex", "cwd": "/work", "failureKind": "provider-unavailable",
				"failureDetail": "Codex API unavailable (503, overloaded)", "failureAt": int64(1),
			}}})
		case "/api/sessions/" + id + "/events":
			_ = json.NewEncoder(response).Encode(map[string]any{"events": []map[string]any{{
				"type": "system", "subtype": "provider_fault", "detail": "Codex API unavailable (503, overloaded)",
			}}})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "cat", id}, strings.NewReader(""), &stdout, &stderr); code != 0 ||
		stdout.String() != "[error]\nCodex API unavailable (503, overloaded)\n" || stderr.Len() != 0 {
		t.Fatalf("cat exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestTerminalCatFallsBackToSessionProviderFault(t *testing.T) {
	const id = "9cd94e86-2222-4333-8444-555555555557"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/sessions":
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []map[string]any{{
				"id": id, "tool": "claude-code", "cwd": "/work", "failureKind": "provider-unavailable",
				"failureDetail": "Claude API overloaded (529)", "failureAt": int64(1),
			}}})
		case "/api/sessions/" + id + "/events":
			_ = json.NewEncoder(response).Encode(map[string]any{"events": []any{}})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "transcript", id}, strings.NewReader(""), &stdout, &stderr); code != 0 ||
		stdout.String() != "[error]\nClaude API overloaded (529)\n" || stderr.Len() != 0 {
		t.Fatalf("transcript exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestCatExplainsThatATerminalCodexTranscriptIsStillPending(t *testing.T) {
	const id = "9cd94e86-2222-4333-8444-555555555555"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []map[string]any{{
				"id": id, "name": "review", "tool": "codex", "cwd": "/work", "exited": false,
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions/"+id+"/events":
			_ = json.NewEncoder(response).Encode(map[string]any{"events": []any{}})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "cat", id}, strings.NewReader(""), &stdout, &stderr); code != 0 ||
		stdout.String() != "(waiting for Codex to publish its conversation transcript)\n" || stderr.Len() != 0 {
		t.Fatalf("cat exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestResumeResolvesFriendlySessionsTitle(t *testing.T) {
	const historyID = "762c779a-b891-4966-9e05-26eb796f5208"
	var resumed string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/history":
			_ = json.NewEncoder(response).Encode(map[string]any{
				"schemaVersion": 1,
				"sessions": []map[string]any{{
					"id": historyID, "name": "db-final-review-sol", "tool": "codex",
					"cwd": "/work/db-redo", "conversation_available": true,
				}},
			})
		case request.Method == http.MethodPost && request.URL.Path == "/api/recovery/adopt":
			var body map[string]any
			_ = json.NewDecoder(request.Body).Decode(&body)
			resumed, _ = body["historyId"].(string)
			_, _ = io.WriteString(response, `{"ok":true,"laneId":"resumed-lane","transcriptRecovery":true}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := run(
		[]string{"--host", server.URL, "resume", "db-final-review-sol"},
		strings.NewReader(""), &stdout, &stderr,
	); code != 0 || stdout.String() != "resumed-lane\n" {
		t.Fatalf("resume exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if resumed != historyID {
		t.Fatalf("resumed history = %q, want %q", resumed, historyID)
	}
}

func TestSourceDescribesAndReadsExactProviderFile(t *testing.T) {
	const historyID = "762c779a-b891-4966-9e05-26eb796f5208"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/history":
			_ = json.NewEncoder(response).Encode(map[string]any{
				"schemaVersion": 1,
				"sessions": []map[string]any{{
					"id": historyID, "name": "db-final-review-sol", "tool": "codex",
					"cwd": "/work/db-redo", "conversation_available": true,
				}},
			})
		case request.Method == http.MethodGet && request.URL.Path == "/api/history/"+historyID+"/source":
			_ = json.NewEncoder(response).Encode(map[string]any{
				"schemaVersion": 1,
				"session": map[string]any{
					"id": historyID, "name": "db-final-review-sol", "tool": "codex", "cwd": "/work/db-redo",
				},
				"source_kind": "provider-jsonl", "source_path": "/history/rollout.jsonl",
				"raw_bytes": 321, "raw_available": true, "text_available": true,
			})
		case request.Method == http.MethodGet && request.URL.Path == "/api/history/"+historyID && request.URL.Query().Get("format") == "text":
			_, _ = io.WriteString(response, "[user]\nFinal cold review.\n")
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := run(
		[]string{"--host", server.URL, "source", "db-final-review-sol"},
		strings.NewReader(""), &stdout, &stderr,
	); code != 0 || !strings.Contains(stdout.String(), "/history/rollout.jsonl") ||
		!strings.Contains(stdout.String(), "sessions source ") {
		t.Fatalf("source exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(
		[]string{"--host", server.URL, "source", "db-final-review-sol", "--text"},
		strings.NewReader(""), &stdout, &stderr,
	); code != 0 || stdout.String() != "[user]\nFinal cold review.\n" {
		t.Fatalf("source text exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
