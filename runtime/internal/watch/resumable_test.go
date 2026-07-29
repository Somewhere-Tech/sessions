package watch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScanResumableConversationsIncludesCodexAndDeduplicatesRollouts(t *testing.T) {
	home := t.TempDir()
	setFixtureHome(t, home)
	root := filepath.Join(home, ".codex", "sessions", "2026", "07", "22")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	id := "11111111-1111-4111-8111-111111111111"
	cwd := filepath.Join(home, "work")
	meta, err := json.Marshal(map[string]any{
		"timestamp": "2026-07-22T08:00:00Z",
		"type":      "session_meta",
		"payload": map[string]any{
			"id": id, "cwd": cwd, "originator": "Codex Desktop",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := json.Marshal(map[string]any{
		"timestamp": "2026-07-22T08:00:01Z",
		"type":      "response_item",
		"payload": map[string]any{
			"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": "Improve the Sessions handoff"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	first := string(meta) + "\n" + string(message) + "\n"
	older := filepath.Join(root, "rollout-old.jsonl")
	newer := filepath.Join(root, "rollout-new.jsonl")
	if err := os.WriteFile(older, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newer, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(older, mustTime(t, "2026-07-22T08:00:00Z"), mustTime(t, "2026-07-22T08:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, mustTime(t, "2026-07-22T09:00:00Z"), mustTime(t, "2026-07-22T09:00:00Z")); err != nil {
		t.Fatal(err)
	}

	conversations := ScanResumableConversations()
	if len(conversations) != 1 {
		t.Fatalf("conversations = %#v", conversations)
	}
	got := conversations[0]
	if got.Tool != "codex" || got.SessionID != id || got.Cwd != cwd || got.Origin != "Codex Desktop" ||
		got.FirstUserMessage != "Improve the Sessions handoff" {
		t.Fatalf("codex conversation = %#v", got)
	}
}

func TestScanResumableConversationsReadsClaudeTitleWithoutExposingPath(t *testing.T) {
	home := t.TempDir()
	setFixtureHome(t, home)
	cwd := filepath.Join(home, "bolo")
	id := "22222222-2222-4222-8222-222222222222"
	path := filepath.Join(home, ".claude", "projects", EncodeClaudeCWD(cwd), id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	contents := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"Build the product launch plan"}}`,
		`{"type":"ai-title","aiTitle":"Generic launch work"}`,
		`{"type":"custom-title","customTitle":"BOLO"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	conversations := ScanResumableConversations()
	if len(conversations) != 1 {
		t.Fatalf("conversations = %#v", conversations)
	}
	got := conversations[0]
	if got.Title != "BOLO" || got.FirstUserMessage != "Build the product launch plan" || got.SourcePath != path {
		t.Fatalf("Claude conversation = %#v", got)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), path) || strings.Contains(string(encoded), "SourcePath") {
		t.Fatalf("provider source path leaked into JSON: %s", encoded)
	}
}

func setFixtureHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
