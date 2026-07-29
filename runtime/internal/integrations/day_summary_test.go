package integrations

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSummarizeConversationDaySkipsCodexForkSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fork.jsonl")
	encoded := `{"timestamp":"2026-07-22T08:00:00Z","type":"session_meta","payload":{"id":"child","cwd":"/work/sessions","source":{"subagent":{"thread_spawn":{"parent_thread_id":"parent"}}}}}
{"timestamp":"2026-07-22T08:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Copied parent request"}]}}
{"timestamp":"2026-07-22T08:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Copied parent answer"}]}}
{"timestamp":"2026-07-22T09:00:00Z","type":"event_msg","payload":{"type":"task_started","turn_id":"child-turn"}}
{"timestamp":"2026-07-22T09:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Audit the usage scanner"}]}}
{"timestamp":"2026-07-22T09:05:00Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Found and fixed replay inflation"}]}}
`
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.July, 22, 0, 0, 0, 0, time.UTC)
	summary, err := SummarizeConversationDay(path, "codex", start, start.AddDate(0, 0, 1), []string{"child-turn"})
	if err != nil {
		t.Fatal(err)
	}
	if summary.CWD != "/work/sessions" || summary.Origin != "Codex child agent" {
		t.Fatalf("identity = %#v", summary)
	}
	if summary.FirstUser != "Audit the usage scanner" || summary.LastAssistant != "Found and fixed replay inflation" || summary.MessageCount != 2 {
		t.Fatalf("summary included copied parent context: %#v", summary)
	}
}

func TestSummarizeConversationDayFiltersClaudeByTimestamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.jsonl")
	encoded := `{"type":"user","timestamp":"2026-07-21T23:59:00Z","cwd":"/work/old","message":{"role":"user","content":"Yesterday"}}
{"type":"user","timestamp":"2026-07-22T10:00:00Z","cwd":"/work/sessions","message":{"role":"user","content":"Improve Today"}}
{"type":"assistant","timestamp":"2026-07-22T10:04:00Z","cwd":"/work/sessions","message":{"role":"assistant","content":[{"type":"text","text":"Today now includes external work"}]}}
`
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.July, 22, 0, 0, 0, 0, time.UTC)
	summary, err := SummarizeConversationDay(path, "claude", start, start.AddDate(0, 0, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.FirstUser != "Improve Today" || summary.LastAssistant != "Today now includes external work" || summary.MessageCount != 2 {
		t.Fatalf("summary = %#v", summary)
	}
}
