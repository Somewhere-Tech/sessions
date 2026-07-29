package watch

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexWatcherBoundedBackfillAndExactLiveHandoff(t *testing.T) {
	root := t.TempDir()
	rolloutPath := filepath.Join(root, "sessions", "2026", "07", "16", "rollout-fixture.jsonl")
	if err := os.MkdirAll(filepath.Dir(rolloutPath), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(rolloutPath)
	if err != nil {
		t.Fatal(err)
	}
	writer := bufio.NewWriter(file)
	const fixtureLines = 2_105
	for index := 0; index < fixtureLines; index++ {
		record := map[string]any{
			"timestamp": "2026-07-16T12:00:00Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []any{map[string]any{
					"type": "output_text", "text": fmt.Sprintf("backfill-%04d", index),
				}},
			},
		}
		encoded, marshalErr := json.Marshal(record)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if _, writeErr := writer.Write(append(encoded, '\n')); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	watcher := WatchCodexRollout(CodexWatcherOptions{
		CreatedAt:    time.Date(2026, time.July, 16, 12, 0, 0, 0, time.Local),
		SessionsDir:  filepath.Join(root, "sessions"),
		RolloutPath:  rolloutPath,
		InitialDelay: time.Millisecond,
		PollInterval: 10 * time.Millisecond,
	})
	defer watcher.Close()

	backfill := collectEvents(t, watcher.Events, codexBackfillLineLimit, 3*time.Second)
	if got := eventText(backfill[0]); got != "backfill-0105" {
		t.Fatalf("first bounded event text = %q, want %q", got, "backfill-0105")
	}
	if got := eventText(backfill[len(backfill)-1]); got != "backfill-2104" {
		t.Fatalf("last bounded event text = %q, want %q", got, "backfill-2104")
	}
	if watcher.Path() != rolloutPath {
		t.Fatalf("watcher path = %q, want %q", watcher.Path(), rolloutPath)
	}

	marker := "sessions-backfill-handoff-marker"
	appendFile, err := os.OpenFile(rolloutPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	appendEncoder := json.NewEncoder(appendFile)
	if err := appendEncoder.Encode(map[string]any{
		"timestamp": "2026-07-16T12:01:00Z",
		"type":      "response_item",
		"payload": map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []any{map[string]any{
				"type": "output_text", "text": marker,
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := appendEncoder.Encode(map[string]any{
		"timestamp": "2026-07-16T12:01:01Z",
		"type":      "event_msg",
		"payload":   map[string]any{"type": "task_started"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := appendFile.Close(); err != nil {
		t.Fatal(err)
	}

	live := collectEvents(t, watcher.Events, 1, 2*time.Second)
	if got := eventText(live[0]); got != marker {
		t.Fatalf("live event text = %q, want %q", got, marker)
	}
	select {
	case working := <-watcher.Working:
		if !working {
			t.Fatal("task_started working state = false, want true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for task_started working state")
	}
	assertNoEvent(t, watcher.Events, 100*time.Millisecond)
	select {
	case duplicate := <-watcher.Working:
		t.Fatalf("duplicate working transition: %v", duplicate)
	case <-time.After(100 * time.Millisecond):
	}
	t.Logf("bounded replay events: %d; appended marker emissions: 1; appended working transitions: 1", len(backfill))
}

func TestCodexWatcherPreservesPartialRecordAtHandoff(t *testing.T) {
	root := t.TempDir()
	rolloutPath := filepath.Join(root, "sessions", "2026", "07", "16", "rollout-partial.jsonl")
	if err := os.MkdirAll(filepath.Dir(rolloutPath), 0o755); err != nil {
		t.Fatal(err)
	}
	complete := codexAssistantRecord(t, "complete-before-boundary")
	partial := codexAssistantRecord(t, "partial-at-boundary")
	contents := append(append(append([]byte{}, complete...), '\n'), partial...)
	if err := os.WriteFile(rolloutPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}

	watcher := WatchCodexRollout(CodexWatcherOptions{
		CreatedAt:    time.Date(2026, time.July, 16, 12, 0, 0, 0, time.Local),
		SessionsDir:  filepath.Join(root, "sessions"),
		RolloutPath:  rolloutPath,
		InitialDelay: time.Millisecond,
		PollInterval: 10 * time.Millisecond,
	})
	defer watcher.Close()

	backfill := collectEvents(t, watcher.Events, 1, 2*time.Second)
	if got := eventText(backfill[0]); got != "complete-before-boundary" {
		t.Fatalf("backfill event text = %q", got)
	}
	appendFile, err := os.OpenFile(rolloutPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appendFile.Write([]byte("\n")); err != nil {
		appendFile.Close()
		t.Fatal(err)
	}
	if err := appendFile.Close(); err != nil {
		t.Fatal(err)
	}

	live := collectEvents(t, watcher.Events, 1, 2*time.Second)
	if got := eventText(live[0]); got != "partial-at-boundary" {
		t.Fatalf("completed partial event text = %q", got)
	}
	assertNoEvent(t, watcher.Events, 100*time.Millisecond)
}

func codexAssistantRecord(t *testing.T, text string) []byte {
	t.Helper()
	record, err := json.Marshal(map[string]any{
		"timestamp": "2026-07-16T12:00:00Z",
		"type":      "response_item",
		"payload": map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []any{map[string]any{
				"type": "output_text", "text": text,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestBoundedBackfillStart(t *testing.T) {
	var data []byte
	for index := 0; index < codexBackfillLineLimit+5; index++ {
		data = append(data, []byte(fmt.Sprintf("line-%d\n", index))...)
	}
	start := boundedBackfillStart(data, 0)
	scanner := bufio.NewScanner(strings.NewReader(string(data[start:])))
	count := 0
	for scanner.Scan() {
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if count != codexBackfillLineLimit {
		t.Fatalf("bounded line count = %d, want %d", count, codexBackfillLineLimit)
	}
}
