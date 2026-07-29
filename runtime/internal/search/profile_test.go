package search

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

func TestProfileConversationIsAvailableToTranscriptAndSearch(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "project")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(root, "profiles", "claude", "work")
	const sessionID = "cccccccc-1111-4222-8333-444444444444"
	path := filepath.Join(configDir, "projects", watch.EncodeClaudeCWD(cwd), sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	conversation := "" +
		`{"type":"user","timestamp":"2026-07-19T12:00:00Z","message":{"role":"user","content":"find the profile needle"}}` + "\n" +
		`{"type":"assistant","timestamp":"2026-07-19T12:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"profile needle found"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(conversation), 0o600); err != nil {
		t.Fatal(err)
	}
	live := []state.SessionInfo{{
		ID: sessionID, Name: "profile recall", Cmd: "claude", Tool: state.ToolClaude,
		Cwd: cwd, Profile: "work", ConfigDir: configDir,
		Args: []string{"--session-id", sessionID}, CreatedAt: time.Now().Add(-time.Minute).UnixMilli(),
	}}
	history := integrations.NewHistoryStore(integrations.HistoryOptions{RunnerStateDir: filepath.Join(root, "runners")})
	transcript, err := history.Transcript(live, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Messages) != 2 || transcript.Messages[0].Text != "find the profile needle" ||
		transcript.Messages[1].Text != "profile needle found" {
		t.Fatalf("profile transcript = %#v", transcript.Messages)
	}
	result, err := Run(context.Background(), history, live, Options{Query: "profile needle"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 || len(result.Matches) != 2 || result.Matches[0].SessionID != sessionID {
		t.Fatalf("profile search = %#v", result)
	}
}

func TestSearchFindsClaudePromptHistoryAfterFullTranscriptIsGone(t *testing.T) {
	root := t.TempDir()
	claudeDir := filepath.Join(root, ".claude", "projects")
	codexDir := filepath.Join(root, "codex")
	for _, dir := range []string{claudeDir, codexDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	id := "55555555-5555-4555-8555-555555555555"
	historyPath := filepath.Join(root, ".claude", "history.jsonl")
	if err := os.WriteFile(historyPath, []byte(
		`{"display":"Build the BOLO application","project":"/work/bolo","sessionId":"`+id+`","timestamp":1000}`+"\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	history := integrations.NewHistoryStore(integrations.HistoryOptions{
		RunnerStateDir: filepath.Join(root, "runners"), ClaudeProjectsDir: claudeDir,
		ClaudeHistoryPath: historyPath, CodexSessionsDir: codexDir, DiscoverProviderHistory: true,
	})
	result, err := Run(context.Background(), history, nil, Options{Query: "BOLO"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || result.Matches[0].SessionID != "provider-history:claude:"+id ||
		result.Matches[0].Snippet == "" || result.Matches[0].ProviderSessionID != id {
		t.Fatalf("prompt-history search = %#v", result)
	}
}
