package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
)

func TestRecallCLIIsThinViewOfIntegrationEndpoints(t *testing.T) {
	const id = "11111111-2222-4333-8444-555555555555"
	session := integrations.HistorySession{
		ID: id, Name: "fixture recall", Tool: "claude", CWD: "/fixture",
		Machine: "fixture-mac", CreatedAt: 1, LastActivityAt: 2,
		MessageCount: 1, ConversationAvailable: true,
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/history":
			_ = json.NewEncoder(response).Encode(integrations.HistoryResponse{
				SchemaVersion: integrations.SchemaVersion, Sessions: []integrations.HistorySession{session},
			})
		case "/api/history/" + id:
			if request.URL.Query().Get("format") == "text" {
				_, _ = io.WriteString(response, "[user]\nFixture recall text\n")
				return
			}
			_ = json.NewEncoder(response).Encode(integrations.TranscriptResponse{
				SchemaVersion: integrations.SchemaVersion, Session: session,
				Messages: []integrations.TranscriptMessage{{Role: "user", Text: "Fixture recall text"}},
			})
		case "/api/history/" + id + "/raw":
			_, _ = io.WriteString(response, "fixture raw bytes\n")
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "list", args: []string{"--host", server.URL, "recall"}, want: "11111111  claude  1 messages  available  fixture recall\n"},
		{name: "text", args: []string{"--host", server.URL, "recall", id}, want: "[user]\nFixture recall text\n"},
		{name: "raw", args: []string{"--host", server.URL, "recall", id, "--raw"}, want: "fixture raw bytes\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(test.args, strings.NewReader(""), &stdout, &stderr); code != 0 {
				t.Fatalf("exit=%d stderr=%q", code, stderr.String())
			}
			if stdout.String() != test.want {
				t.Fatalf("stdout=%q, want %q", stdout.String(), test.want)
			}
		})
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "--json", "recall", id}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("json exit=%d stderr=%q", code, stderr.String())
	}
	var transcript integrations.TranscriptResponse
	if err := json.Unmarshal(stdout.Bytes(), &transcript); err != nil {
		t.Fatal(err)
	}
	if transcript.SchemaVersion != integrations.SchemaVersion || len(transcript.Messages) != 1 ||
		transcript.Messages[0].Text != "Fixture recall text" {
		t.Fatalf("transcript = %#v", transcript)
	}
}
