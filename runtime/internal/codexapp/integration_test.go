package codexapp

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// Run explicitly with:
//
//	CODEXAPP_INTEGRATION=1 go test -v ./internal/codexapp -run TestRealAppServerTurn
func TestRealAppServerTurn(t *testing.T) {
	if os.Getenv("CODEXAPP_INTEGRATION") != "1" {
		t.Skip("set CODEXAPP_INTEGRATION=1 to spend a real Codex turn")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	scratch, err := os.MkdirTemp("/tmp", "sessions-codexapp-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(scratch)

	clientOptions := Options{}
	if socketPath := os.Getenv("CODEXAPP_SOCKET"); socketPath != "" {
		clientOptions.SkipDaemonStart = true
		clientOptions.SocketPath = socketPath
	}
	client, err := NewClient(ctx, clientOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	conversationID, err := client.NewConversation(ctx, ConversationOptions{CWD: scratch})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("CONVERSATION_ID %s", conversationID)
	t.Logf("SCRATCH_CWD %s", scratch)

	stream, err := client.SendUserTurn(ctx, conversationID, "Reply with exactly APPSERVER_OK.")
	if err != nil {
		t.Fatal(err)
	}
	var deltas strings.Builder
	var sawTokenCount, sawTurnComplete bool
	sequence := 0
	for event := range stream.Events {
		sequence++
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("EVENT %02d %T %s", sequence, event, encoded)
		switch event := event.(type) {
		case AgentMessageDelta:
			deltas.WriteString(event.Delta)
		case TokenCount:
			if event.Usage.Last.TotalTokens > 0 {
				sawTokenCount = true
			}
		case TurnComplete:
			sawTurnComplete = event.Status == "completed"
		}
	}
	result, err := stream.Result(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("RESULT %s", mustJSON(t, result))
	if deltas.String() != "APPSERVER_OK" {
		t.Fatalf("structured agent-message deltas = %q", deltas.String())
	}
	if result.Message != "APPSERVER_OK" {
		t.Fatalf("final assistant message = %q", result.Message)
	}
	if !sawTokenCount || result.TokenUsage.Last.TotalTokens <= 0 {
		t.Fatalf("missing positive token count: %+v", result.TokenUsage)
	}
	if !sawTurnComplete || result.Status != "completed" {
		t.Fatalf("missing completed turn: %+v", result)
	}
}

// Run explicitly with:
//
//	CODEXAPP_INTEGRATION=1 go test -v ./internal/codexapp -run TestRealAppServerModelCatalog
func TestRealAppServerModelCatalog(t *testing.T) {
	if os.Getenv("CODEXAPP_INTEGRATION") != "1" {
		t.Skip("set CODEXAPP_INTEGRATION=1 to inspect the real Codex model catalog")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clientOptions := Options{}
	if socketPath := os.Getenv("CODEXAPP_SOCKET"); socketPath != "" {
		clientOptions.SkipDaemonStart = true
		clientOptions.SocketPath = socketPath
	}
	client, err := NewClient(ctx, clientOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	catalog, err := client.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog) == 0 {
		t.Fatal("real app-server returned an empty model catalog")
	}
	for _, model := range catalog {
		if model.ID == "" {
			t.Fatalf("catalog contains an empty model id: %#v", model)
		}
	}
	t.Logf("CATALOG %s", mustJSON(t, catalog))
	const invalid = "sessions-model-that-must-not-exist"
	if resolved, err := ResolveModelChoice(catalog, ModelChoice{Model: invalid}); err == nil || !strings.Contains(err.Error(), `model "`+invalid+`" not available`) {
		t.Fatalf("invalid model resolved to %#v with error %v", resolved, err)
	} else {
		t.Logf("INVALID_REJECTED %s", err)
	}
}

// Run explicitly with:
//
//	CODEXAPP_IMPORT_INTEGRATION=1 go test -v ./internal/codexapp -run TestRealAppServerHistoryImport
//
// This creates a local Codex thread but does not start a model turn.
func TestRealAppServerHistoryImport(t *testing.T) {
	if os.Getenv("CODEXAPP_IMPORT_INTEGRATION") != "1" {
		t.Skip("set CODEXAPP_IMPORT_INTEGRATION=1 to create a local imported Codex thread")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := NewClient(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	conversationID, err := client.NewConversation(ctx, ConversationOptions{
		CWD: "/tmp", DeveloperInstructions: "This thread contains an explicit Sessions import integration fixture.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.InjectMessages(ctx, conversationID, []ImportedMessage{
		{Role: "user", Text: "Sessions import integration question."},
		{Role: "assistant", Text: "Sessions import integration answer."},
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("IMPORTED_CONVERSATION_ID %s", conversationID)
}

func TestRealAppServerFallbackHandshake(t *testing.T) {
	if os.Getenv("CODEXAPP_FALLBACK_INTEGRATION") != "1" {
		t.Skip("set CODEXAPP_FALLBACK_INTEGRATION=1 to test the unmanaged-install fallback")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := NewClient(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if endpoint := client.RemoteEndpoint(); !strings.HasPrefix(endpoint, "unix:///tmp/sessions-appserver-") {
		t.Fatalf("fallback endpoint = %q", endpoint)
	} else {
		t.Logf("REMOTE_ENDPOINT %s", endpoint)
	}
	conversationID, err := client.NewConversation(ctx, ConversationOptions{CWD: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("CONVERSATION_ID %s", conversationID)
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
