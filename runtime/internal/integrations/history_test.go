package integrations

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/backup"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

func TestTranscriptNormalizationStopsWhenSearchIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := normalizeTranscriptReaderContext(ctx, bufio.NewReader(strings.NewReader(
		`{"message":{"role":"user","content":"should not be parsed"}}`+"\n",
	)), "fixture.jsonl", "claude")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context canceled", err)
	}
}

func TestTranscriptNormalizesProviderFaultSystemEvent(t *testing.T) {
	messages := transcriptMessages(map[string]any{
		"type": "system", "subtype": "provider_fault", "detail": "Codex API unavailable (503, overloaded)",
		"timestamp": "2026-09-03T12:00:00Z",
	}, map[string]string{})
	if len(messages) != 1 || messages[0].Role != "error" || messages[0].Kind != "provider_fault" ||
		messages[0].Text != "Codex API unavailable (503, overloaded)" {
		t.Fatalf("provider fault messages = %#v", messages)
	}
}

func TestHistoryNormalizesCodexRolloutThroughWatchContract(t *testing.T) {
	root := t.TempDir()
	runnerDir := filepath.Join(root, "runners")
	sessionsDir := filepath.Join(root, "codex-sessions")
	cwd := filepath.Join(root, "worktree")
	for _, dir := range []string{runnerDir, sessionsDir, cwd} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, time.July, 16, 20, 0, 0, 0, time.UTC)
	id := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	resumeID := "01234567-89ab-4cde-8fab-0123456789ab"
	metadataPath := filepath.Join(runnerDir, id+".json")
	if err := state.WriteMetadata(metadataPath, state.Metadata{
		ID: id, Name: "codex recall", Cmd: "codex", Args: []string{"resume", resumeID},
		Cwd: cwd, CreatedAt: now.Add(-time.Minute).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	rolloutPath := filepath.Join(sessionsDir, "2026", "07", "16", "rollout-"+resumeID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(rolloutPath), 0o700); err != nil {
		t.Fatal(err)
	}
	lines := []map[string]any{
		{"timestamp": now.Add(-time.Minute).Format(time.RFC3339Nano), "type": "session_meta", "payload": map[string]any{
			"cwd": cwd, "timestamp": now.Add(-time.Minute).Format(time.RFC3339Nano),
		}},
		{"timestamp": now.Format(time.RFC3339Nano), "type": "response_item", "payload": map[string]any{
			"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Codex fixture question"}},
		}},
		{"timestamp": now.Add(time.Second).Format(time.RFC3339Nano), "type": "response_item", "payload": map[string]any{
			"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Codex fixture answer"}},
		}},
		{"timestamp": now.Add(2 * time.Second).Format(time.RFC3339Nano), "type": "event_msg", "payload": map[string]any{"type": "task_complete"}},
	}
	file, err := os.OpenFile(rolloutPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for _, line := range lines {
		if err := encoder.Encode(line); err != nil {
			file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	store := NewHistoryStore(HistoryOptions{
		RunnerStateDir: runnerDir, CodexSessionsDir: sessionsDir, Machine: "fixture-mac",
		Now: func() time.Time { return now },
	})
	history, err := store.List(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Sessions) != 1 || history.Sessions[0].Tool != "codex" ||
		history.Sessions[0].ProviderSessionID != resumeID ||
		history.Sessions[0].MessageCount != 2 || !history.Sessions[0].ConversationAvailable {
		t.Fatalf("history = %#v", history)
	}
	transcript, err := store.Transcript(nil, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Messages) != 2 || transcript.Messages[0].Role != "user" ||
		transcript.Messages[0].Text != "Codex fixture question" ||
		transcript.Messages[1].Role != "assistant" || transcript.Messages[1].Text != "Codex fixture answer" {
		t.Fatalf("messages = %#v", transcript.Messages)
	}
	raw, err := os.ReadFile(rolloutPath)
	if err != nil {
		t.Fatal(err)
	}
	cut := bytes.Index(raw, []byte(`"role":"assistant"`))
	if cut < 1 {
		t.Fatalf("assistant record missing from fixture: %s", raw)
	}
	limited, err := store.TranscriptLimited(nil, id, int64(cut))
	if err != nil {
		t.Fatal(err)
	}
	if len(limited.Messages) != 1 || limited.Messages[0].Text != "Codex fixture question" {
		t.Fatalf("bounded messages = %#v", limited.Messages)
	}
}

func TestHistoryRecoversCodexProviderIdentityFromResolvedRollout(t *testing.T) {
	root := t.TempDir()
	runnerDir := filepath.Join(root, "runners")
	sessionsDir := filepath.Join(root, "codex-sessions")
	cwd := filepath.Join(root, "worktree")
	for _, dir := range []string{runnerDir, sessionsDir, cwd} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, time.August, 4, 18, 0, 0, 0, time.UTC)
	laneID := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	providerID := "01234567-89ab-4cde-8fab-0123456789ab"
	if err := state.WriteMetadata(filepath.Join(runnerDir, laneID+".json"), state.Metadata{
		ID: laneID, Name: "db-final-review-sol", Cmd: "codex", Cwd: cwd,
		CreatedAt: now.Add(-time.Minute).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	rolloutPath := filepath.Join(sessionsDir, "2026", "08", "04", "rollout-"+providerID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(rolloutPath), 0o700); err != nil {
		t.Fatal(err)
	}
	conversation := strings.Join([]string{
		`{"timestamp":"2026-08-04T17:59:00Z","type":"session_meta","payload":{"id":"` + providerID + `","cwd":"` + cwd + `","timestamp":"2026-08-04T17:59:00Z"}}`,
		`{"timestamp":"2026-08-04T18:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Final cold review"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(rolloutPath, []byte(conversation), 0o600); err != nil {
		t.Fatal(err)
	}

	store := NewHistoryStore(HistoryOptions{
		RunnerStateDir: runnerDir, CodexSessionsDir: sessionsDir, Machine: "fixture-mac",
		Now: func() time.Time { return now },
	})
	history, err := store.Lookup(nil, laneID)
	if err != nil {
		t.Fatal(err)
	}
	if history.ProviderSessionID != providerID || !history.ConversationAvailable {
		t.Fatalf("history = %#v", history)
	}
	source, err := store.Source(nil, laneID)
	if err != nil {
		t.Fatal(err)
	}
	if source.SourcePath != rolloutPath || source.SourceKind != "provider-jsonl" ||
		!source.RawAvailable || !source.TextAvailable || source.RawBytes != int64(len(conversation)) {
		t.Fatalf("source = %#v", source)
	}
}

func TestHistoryUsesHumanTitleAndProviderConversationIdentity(t *testing.T) {
	claudeID := "11111111-2222-4333-8444-555555555555"
	codexID := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	tests := []struct {
		name     string
		source   backup.Session
		tool     string
		wantName string
		wantID   string
	}{
		{
			name: "generic Claude name yields to generated title",
			source: backup.Session{
				Name: "Claude - somewhere web", ClaudeAITitle: "Lakebuild product direction",
				ClaudeSessionID: claudeID,
			},
			tool: "claude", wantName: "Lakebuild product direction", wantID: claudeID,
		},
		{
			name: "explicit name remains authoritative",
			source: backup.Session{
				Name: "PM", Description: "A much longer first request",
				ClaudeSessionID: claudeID,
			},
			tool: "claude", wantName: "PM", wantID: claudeID,
		},
		{
			name: "legacy Codex resume args preserve conversation id",
			source: backup.Session{
				Name: "Codex - tech", Description: "Windows release review",
				Args: []string{"resume", codexID},
			},
			tool: "codex", wantName: "Windows release review", wantID: codexID,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := historyDisplayName(test.source); got != test.wantName {
				t.Fatalf("name=%q, want %q", got, test.wantName)
			}
			if got := providerSessionID(test.source, test.tool); got != test.wantID {
				t.Fatalf("provider session=%q, want %q", got, test.wantID)
			}
		})
	}
}

func TestHistoryIncludesUnmanagedProviderConversationByTitle(t *testing.T) {
	root := t.TempDir()
	claudeDir := filepath.Join(root, "claude-projects")
	codexDir := filepath.Join(root, "codex-sessions")
	cwd := filepath.Join(root, "bolo")
	id := "33333333-3333-4333-8333-333333333333"
	path := filepath.Join(claudeDir, watch.EncodeClaudeCWD(cwd), id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	contents := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"The private launch phrase"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":"I found the launch details."}}`,
		`{"type":"custom-title","customTitle":"BOLO"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewHistoryStore(HistoryOptions{
		RunnerStateDir: filepath.Join(root, "runners"), ClaudeProjectsDir: claudeDir,
		CodexSessionsDir: codexDir, Machine: "fixture-mac", DiscoverProviderHistory: true,
	})
	history, err := store.List(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Sessions) != 1 {
		t.Fatalf("history = %#v", history)
	}
	session := history.Sessions[0]
	if session.Name != "BOLO" || session.ProviderSessionID != id || !session.External ||
		session.ID != "provider:claude:"+id || session.MessageCount != 2 {
		t.Fatalf("provider history = %#v", session)
	}
	transcript, err := store.Transcript(nil, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Messages) != 2 || transcript.Messages[0].Text != "The private launch phrase" {
		t.Fatalf("provider transcript = %#v", transcript)
	}
}

func TestHistoryIncludesPromptIndexWhenProviderTranscriptIsGone(t *testing.T) {
	root := t.TempDir()
	claudeDir := filepath.Join(root, ".claude", "projects")
	codexDir := filepath.Join(root, "codex-sessions")
	historyPath := filepath.Join(root, ".claude", "history.jsonl")
	for _, dir := range []string{claudeDir, codexDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	id := "44444444-4444-4444-8444-444444444444"
	contents := "" +
		`{"display":"Build the BOLO app","project":"/work/bolo","sessionId":"` + id + `","timestamp":1000}` + "\n" +
		`{"display":"Fix its login flow","project":"/work/bolo","sessionId":"` + id + `","timestamp":2000}` + "\n"
	if err := os.WriteFile(historyPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewHistoryStore(HistoryOptions{
		RunnerStateDir: filepath.Join(root, "runners"), ClaudeProjectsDir: claudeDir,
		ClaudeHistoryPath: historyPath, CodexSessionsDir: codexDir, Machine: "fixture-mac",
		DiscoverProviderHistory: true,
	})
	history, err := store.List(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Sessions) != 1 {
		t.Fatalf("history = %#v", history)
	}
	session := history.Sessions[0]
	if session.ID != "provider-history:claude:"+id || !session.PromptHistoryOnly ||
		!session.ConversationAvailable || session.MessageCount != 2 {
		t.Fatalf("prompt history session = %#v", session)
	}
	transcript, err := store.Transcript(nil, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Messages) != 2 || transcript.Messages[0].Role != "user" ||
		transcript.Messages[1].Text != "Fix its login flow" {
		t.Fatalf("prompt transcript = %#v", transcript)
	}
}

func TestTranscriptPreviewReturnsBoundedTail(t *testing.T) {
	root := t.TempDir()
	runnerDir := filepath.Join(root, "runners")
	claudeDir := filepath.Join(root, "claude-projects")
	cwd := filepath.Join(root, "worktree")
	for _, dir := range []string{runnerDir, claudeDir, cwd} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	id := "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"
	if err := state.WriteMetadata(filepath.Join(runnerDir, id+".json"), state.Metadata{
		ID: id, Name: "preview", Cmd: "claude", Args: []string{"--session-id", id}, Cwd: cwd,
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(claudeDir, watch.EncodeClaudeCWD(cwd), id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for index := range 600 {
		lines = append(lines, fmt.Sprintf(`{"type":"user","message":{"role":"user","content":"message-%d"}}`, index))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewHistoryStore(HistoryOptions{RunnerStateDir: runnerDir, ClaudeProjectsDir: claudeDir})
	preview, err := store.TranscriptPreview(nil, id, 1<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Truncated || len(preview.Messages) != 3 || preview.Messages[0].Text != "message-597" || preview.Messages[2].Text != "message-599" {
		t.Fatalf("message-bounded preview=%#v", preview)
	}
	preview, err = store.TranscriptPreview(nil, id, int64(len(lines[len(lines)-1])+2), 20)
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Truncated || len(preview.Messages) != 1 || preview.Messages[0].Text != "message-599" {
		t.Fatalf("byte-bounded preview=%#v", preview)
	}
	window, err := store.TranscriptWindow(nil, id, TranscriptWindowOptions{End: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(window.Messages) != MaxTranscriptWindowSpan || window.Session.MessageCount != 600 ||
		!window.HasMore || window.NextIndex != MaxTranscriptWindowSpan ||
		window.Messages[0].Index != 0 || window.Messages[len(window.Messages)-1].Index != 499 {
		t.Fatalf("paged window=%#v", window)
	}
}

func TestTranscriptIndexesMessagesAndExpandsOnlySearchableRelayPayloads(t *testing.T) {
	records := []map[string]any{
		{"timestamp": "2026-07-23T20:00:00Z", "message": map[string]any{"role": "user", "content": "Find my drafts direction"}},
		{"timestamp": "2026-07-23T20:00:10Z", "message": map[string]any{"role": "user", "content": "# AGENTS.md instructions\n\n<INSTRUCTIONS>internal repository context</INSTRUCTIONS>"}},
		{"timestamp": "2026-07-23T20:00:20Z", "message": map[string]any{"role": "user", "content": "# CLAUDE.md instructions\n\ninternal provider context"}},
		{"timestamp": "2026-07-23T20:00:30Z", "message": map[string]any{"role": "user", "content": "<local-command-caveat>internal provider control text</local-command-caveat>"}},
		{"timestamp": "2026-07-23T20:00:45Z", "message": map[string]any{"role": "user", "content": "<command-name>/model</command-name>"}},
		{"timestamp": "2026-07-23T20:00:50Z", "message": map[string]any{"role": "assistant", "content": "<local-command-stdout>provider control output</local-command-stdout>"}},
		{"timestamp": "2026-07-23T20:01:00Z", "message": map[string]any{"role": "user", "content": "<task-notification>child finished</task-notification>"}},
		{"timestamp": "2026-07-23T20:01:30Z", "message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "<system-reminder>hidden provider text</system-reminder>"},
			map[string]any{"type": "tool_use", "id": "relay-hidden", "name": "mcp__sessions__send_message", "input": map[string]any{
				"target": "reviewer", "message": "Keep the relay request even when its sibling text is hidden",
			}},
		}}},
		{"timestamp": "2026-07-23T20:02:00Z", "message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "relay-1", "name": "mcp__sessions__send_message", "input": map[string]any{
				"target": "builder", "message": "Check why hello world failed",
			}},
		}}},
		{"timestamp": "2026-07-23T20:03:00Z", "message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "relay-1", "content": "Builder found a transport timeout"},
		}}},
		{"timestamp": "2026-07-23T20:04:00Z", "message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "relay-2", "name": "Agent", "input": map[string]any{
				"description": "Autopsy the failed build", "prompt": "Reconstruct the founder's hello world directions",
			}},
		}}},
		{"timestamp": "2026-07-23T20:05:00Z", "message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "exec-1", "name": "exec_command", "input": map[string]any{"cmd": "printenv"}},
		}}},
		{"timestamp": "2026-07-23T20:06:00Z", "message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "exec-1", "content": "SECRET=not-indexed"},
		}}},
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	messages, _, err := normalizeTranscriptReader(bufio.NewReader(&encoded), "fixture.jsonl", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 6 {
		t.Fatalf("messages=%#v", messages)
	}
	wantRoles := []string{"user", "tool", "tool", "tool", "tool", "tool"}
	for index, message := range messages {
		if message.Index != index || message.ID == "" || message.Role != wantRoles[index] {
			t.Fatalf("message[%d]=%#v", index, message)
		}
	}
	if messages[1].Kind != "automation" || messages[2].Kind != "handoff" ||
		messages[3].Kind != "handoff" || messages[4].Kind != "handoff" ||
		messages[5].Kind != "delegation" {
		t.Fatalf("message kinds=%#v", messages)
	}
	var joined string
	for _, message := range messages {
		joined += message.Text
	}
	for _, want := range []string{"drafts direction", "child finished", "sibling text is hidden", "hello world failed", "transport timeout", "founder's hello world directions"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("normalized relay text %q does not contain %q", joined, want)
		}
	}
	if strings.Contains(joined, "SECRET") || strings.Contains(joined, "printenv") {
		t.Fatalf("arbitrary tool payload was indexed: %q", joined)
	}
	if strings.Contains(joined, "provider control") || strings.Contains(joined, "command-name") ||
		strings.Contains(joined, "AGENTS.md") || strings.Contains(joined, "CLAUDE.md") {
		t.Fatalf("provider control records leaked into transcript: %q", joined)
	}
}

// writeCodexSession creates one managed Sessions lane whose Codex rollout holds
// the given raw JSONL bytes, so tests can hand the history store deliberately
// torn files.
func writeCodexSession(t *testing.T, runnerDir, sessionsDir, cwd, laneID, providerID, raw string, created time.Time) string {
	t.Helper()
	if err := state.WriteMetadata(filepath.Join(runnerDir, laneID+".json"), state.Metadata{
		ID: laneID, Name: "lane " + laneID[:8], Cmd: "codex", Args: []string{"resume", providerID},
		Cwd: cwd, CreatedAt: created.UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	rolloutPath := filepath.Join(sessionsDir, "2026", "08", "05", "rollout-"+providerID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(rolloutPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rolloutPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return rolloutPath
}

func codexTranscriptLines(cwd string, texts ...string) string {
	lines := []string{
		`{"timestamp":"2026-08-05T09:00:00Z","type":"session_meta","payload":{"cwd":"` + cwd + `","timestamp":"2026-08-05T09:00:00Z"}}`,
	}
	for _, text := range texts {
		lines = append(lines, `{"timestamp":"2026-08-05T09:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"`+text+`"}]}}`)
	}
	return strings.Join(lines, "\n") + "\n"
}

// A torn final record and an undecodable line cost exactly those records. The
// conversation still opens, and the skip is reported rather than hidden.
func TestHistorySkipsTornTranscriptRecordsAndReportsTheSkip(t *testing.T) {
	root := t.TempDir()
	runnerDir := filepath.Join(root, "runners")
	sessionsDir := filepath.Join(root, "codex-sessions")
	cwd := filepath.Join(root, "worktree")
	for _, dir := range []string{runnerDir, sessionsDir, cwd} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, time.August, 5, 9, 0, 0, 0, time.UTC)
	laneID := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	providerID := "01234567-89ab-4cde-8fab-0123456789ab"
	// A valid conversation, then invalid UTF-8, then a record truncated
	// mid-line by a power cut with no trailing newline.
	raw := codexTranscriptLines(cwd, "first question", "second question") +
		"\xff\xfe\x00 not utf-8 and not json\n" +
		`{"timestamp":"2026-08-05T09:00:02Z","type":"response_item","payload":{"type":"mess`
	writeCodexSession(t, runnerDir, sessionsDir, cwd, laneID, providerID, raw, now)

	store := NewHistoryStore(HistoryOptions{
		RunnerStateDir: runnerDir, CodexSessionsDir: sessionsDir, Machine: "fixture-mac",
		Now: func() time.Time { return now },
	})
	history, err := store.List(nil)
	if err != nil {
		t.Fatalf("a torn transcript must not fail the list: %v", err)
	}
	if len(history.Sessions) != 1 {
		t.Fatalf("history = %#v", history)
	}
	session := history.Sessions[0]
	if session.Unreadable || session.MessageCount != 2 || !session.ConversationAvailable {
		t.Fatalf("session = %#v", session)
	}
	if session.SkippedRecords != 2 || history.SkippedRecords != 2 {
		t.Fatalf("skips must be surfaced: session=%d response=%d", session.SkippedRecords, history.SkippedRecords)
	}

	transcript, err := store.Transcript(nil, laneID)
	if err != nil {
		t.Fatalf("a torn transcript must still open: %v", err)
	}
	if len(transcript.Messages) != 2 || transcript.SkippedRecords != 2 {
		t.Fatalf("transcript = %#v", transcript)
	}
	preview, err := store.TranscriptPreview(nil, laneID, 1<<20, 50)
	if err != nil {
		t.Fatalf("preview of a torn transcript: %v", err)
	}
	if len(preview.Messages) != 2 || preview.SkippedRecords != 2 {
		t.Fatalf("preview = %#v", preview)
	}
	window, err := store.TranscriptWindow(nil, laneID, TranscriptWindowOptions{Start: 0, End: 10})
	if err != nil {
		t.Fatalf("window of a torn transcript: %v", err)
	}
	if len(window.Messages) != 2 || window.SkippedRecords != 2 {
		t.Fatalf("window = %#v", window)
	}
	t.Logf("torn transcript: messages=%d skipped=%d", len(transcript.Messages), transcript.SkippedRecords)
}

// One unreadable file must cost one row, never the whole history: a user with a
// single bad transcript keeps the ability to browse every other session.
func TestHistoryListDegradesOneUnreadableSession(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the unreadable-file permissions this test relies on")
	}
	root := t.TempDir()
	runnerDir := filepath.Join(root, "runners")
	sessionsDir := filepath.Join(root, "codex-sessions")
	cwd := filepath.Join(root, "worktree")
	for _, dir := range []string{runnerDir, sessionsDir, cwd} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, time.August, 5, 9, 0, 0, 0, time.UTC)
	healthyLane := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	brokenLane := "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"
	writeCodexSession(t, runnerDir, sessionsDir, cwd, healthyLane,
		"01234567-89ab-4cde-8fab-0123456789ab", codexTranscriptLines(cwd, "healthy question"), now)
	brokenPath := writeCodexSession(t, runnerDir, sessionsDir, cwd, brokenLane,
		"11111111-89ab-4cde-8fab-0123456789ab", codexTranscriptLines(cwd, "lost question"), now)
	// Stat still succeeds; opening the file does not, the way a stale network
	// mount or a revoked ACL behaves.
	if err := os.Chmod(brokenPath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(brokenPath, 0o600) })

	store := NewHistoryStore(HistoryOptions{
		RunnerStateDir: runnerDir, CodexSessionsDir: sessionsDir, Machine: "fixture-mac",
		Now: func() time.Time { return now },
	})
	history, err := store.List(nil)
	if err != nil {
		t.Fatalf("one unreadable transcript emptied the whole history: %v", err)
	}
	if len(history.Sessions) != 2 {
		t.Fatalf("history = %#v", history)
	}
	if history.UnreadableSessions != 1 {
		t.Fatalf("unreadable_sessions = %d, want 1", history.UnreadableSessions)
	}
	byID := make(map[string]HistorySession, len(history.Sessions))
	for _, session := range history.Sessions {
		byID[session.ID] = session
	}
	healthy, ok := byID[healthyLane]
	if !ok || healthy.Unreadable || healthy.MessageCount != 1 || !healthy.ConversationAvailable {
		t.Fatalf("healthy session = %#v", healthy)
	}
	broken, ok := byID[brokenLane]
	if !ok {
		t.Fatalf("the unreadable session disappeared from history: %#v", history.Sessions)
	}
	if !broken.Unreadable || broken.ConversationAvailable {
		t.Fatalf("broken session = %#v", broken)
	}
	if broken.Name == "" || broken.CWD != cwd {
		t.Fatalf("an unreadable session must stay findable: %#v", broken)
	}
	for _, want := range []string{"transcript could not be read", "reachable", "reload history"} {
		if !strings.Contains(broken.UnreadableReason, want) {
			t.Fatalf("reason %q must explain the failure and the next action", broken.UnreadableReason)
		}
	}
	// The same file fetched by id still fails loudly: the caller asked for that
	// one conversation and no partial answer exists.
	if _, err := store.Transcript(nil, brokenLane); err == nil {
		t.Fatal("a direct transcript fetch of an unreadable file must fail")
	}
	t.Logf("degraded row: unreadable=%t reason=%q", broken.Unreadable, broken.UnreadableReason)
}

func TestHistoryMessageCountCacheStaysBounded(t *testing.T) {
	root := t.TempDir()
	store := NewHistoryStore(HistoryOptions{RunnerStateDir: root, Machine: "fixture-mac"})
	path := filepath.Join(root, "transcript.jsonl")
	if err := os.WriteFile(path, []byte(codexTranscriptLines(root, "only question")), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for index := range maxHistoryCacheEntries + 200 {
		// Distinct keys, same bytes: the cache is keyed by path and a daemon
		// sees an unbounded number of paths over its lifetime.
		if _, _, err := store.messageCount(path, "codex", info); err != nil {
			t.Fatal(err)
		}
		store.cacheMu.Lock()
		entry := store.cache[path]
		store.cache[fmt.Sprintf("%s.%d", path, index)] = entry
		store.evictHistoryCacheLocked()
		store.cacheMu.Unlock()
	}
	store.cacheMu.Lock()
	size := len(store.cache)
	store.cacheMu.Unlock()
	if size > maxHistoryCacheEntries {
		t.Fatalf("cache size = %d, want at most %d", size, maxHistoryCacheEntries)
	}
}

// A history record carries two different "when"s and they answer different
// questions. LastActivityAt moves whenever the Sessions record is touched --
// a runner draining its terminal at shutdown, or a metadata rewrite -- while
// ConversationUpdatedAt moves only when the conversation itself was written
// to. Anything ordering conversations by recency needs the second one:
// otherwise a housekeeping pass over a batch of long-finished sessions makes
// all of them look like the most recent thing the user did.
func TestHistoryReportsConversationActivitySeparatelyFromRecordActivity(t *testing.T) {
	root := t.TempDir()
	runnerDir := filepath.Join(root, "runners")
	sessionsDir := filepath.Join(root, "codex-sessions")
	cwd := filepath.Join(root, "worktree")
	for _, dir := range []string{runnerDir, sessionsDir, cwd} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, time.August, 5, 21, 0, 0, 0, time.UTC)
	spokenAt := now.Add(-8 * time.Hour)
	sweptAt := now.Add(-1 * time.Minute)
	id := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	resumeID := "01234567-89ab-4cde-8fab-0123456789ab"
	if err := state.WriteMetadata(filepath.Join(runnerDir, id+".json"), state.Metadata{
		ID: id, Name: "finished lane", Cmd: "codex", Args: []string{"resume", resumeID},
		Cwd: cwd, CreatedAt: spokenAt.Add(-time.Minute).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	rolloutPath := filepath.Join(sessionsDir, "2026", "08", "05", "rollout-"+resumeID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(rolloutPath), 0o700); err != nil {
		t.Fatal(err)
	}
	record := map[string]any{
		"timestamp": spokenAt.Format(time.RFC3339Nano), "type": "response_item",
		"payload": map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "the thing I said"}}},
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rolloutPath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(rolloutPath, spokenAt, spokenAt); err != nil {
		t.Fatal(err)
	}

	store := NewHistoryStore(HistoryOptions{
		RunnerStateDir: runnerDir, CodexSessionsDir: sessionsDir, Machine: "fixture-mac",
		Now: func() time.Time { return now },
	})
	// The daemon reports the record as having been touched by a sweep long
	// after the conversation went quiet.
	history, err := store.List([]state.SessionInfo{{
		ID: id, Cmd: "codex", Args: []string{"resume", resumeID}, Cwd: cwd,
		CreatedAt:  spokenAt.Add(-time.Minute).UnixMilli(),
		LastDataAt: sweptAt.UnixMilli(), Exited: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Sessions) != 1 {
		t.Fatalf("history = %#v", history.Sessions)
	}
	session := history.Sessions[0]
	if session.LastActivityAt != sweptAt.UnixMilli() {
		t.Fatalf("LastActivityAt = %d, want the record's own %d",
			session.LastActivityAt, sweptAt.UnixMilli())
	}
	if session.ConversationUpdatedAt != spokenAt.UnixMilli() {
		t.Fatalf("ConversationUpdatedAt = %d, want the transcript's %d",
			session.ConversationUpdatedAt, spokenAt.UnixMilli())
	}
}
