package watch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClaudeWatcherTailsAPIEventsAndDeduplicatesReread(t *testing.T) {
	projectDir := t.TempDir()
	sessionID := "aaaaaaaa-1111-2222-3333-444444444444"
	path := filepath.Join(projectDir, sessionID+".jsonl")
	initial := []SessionEvent{
		{
			"type":      "user",
			"uuid":      "event-1",
			"timestamp": "2026-07-16T10:00:00Z",
			"message": map[string]any{
				"role":    "user",
				"content": "hello",
			},
		},
		{
			"type": "assistant",
			"uuid": "event-2",
			"message": map[string]any{
				"role":        "assistant",
				"content":     []any{map[string]any{"type": "text", "text": "héllo back"}},
				"stop_reason": "end_turn",
			},
		},
	}
	writeSessionEvents(t, path, initial, false)

	watcher, err := WatchClaudeSession(ClaudeWatcherOptions{
		ClaudeSessionID: sessionID,
		ProjectDir:      projectDir,
		InitialDelay:    time.Millisecond,
		PollInterval:    10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()

	got := collectEvents(t, watcher.Events, len(initial), 2*time.Second)
	assertEventsJSONEqual(t, got, initial)
	if watcher.Path() != path {
		t.Fatalf("watcher path = %q, want %q", watcher.Path(), path)
	}

	third := SessionEvent{
		"type": "system",
		"uuid": "event-3",
		"message": map[string]any{
			"content": "rotated",
		},
	}
	// A truncate/rewrite replays event-1 and event-2, which must be dropped by
	// UUID, while the new event still passes through once.
	writeSessionEvents(t, path, append(append([]SessionEvent{}, initial...), third), false)
	gotThird := collectEvents(t, watcher.Events, 1, 2*time.Second)
	assertEventsJSONEqual(t, gotThird, []SessionEvent{third})
	assertNoEvent(t, watcher.Events, 80*time.Millisecond)
}

func TestClaudeWatcherFindsRealpathProjectForAliasCWD(t *testing.T) {
	home := t.TempDir()
	setFixtureHome(t, home)
	realCWD := filepath.Join(home, "private", "tmp")
	aliasCWD := filepath.Join(home, "tmp")
	if err := os.MkdirAll(realCWD, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realCWD, aliasCWD); err != nil {
		t.Fatal(err)
	}
	const sessionID = "aaaaaaaa-1111-2222-3333-444444444444"
	projectDir, err := ClaudeProjectDir(aliasCWD)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(projectDir, sessionID+".jsonl")
	event := SessionEvent{"type": "assistant", "uuid": "realpath-event"}
	writeSessionEvents(t, path, []SessionEvent{event}, false)

	watcher, err := WatchClaudeSession(ClaudeWatcherOptions{
		CWD: aliasCWD, ClaudeSessionID: sessionID,
		InitialDelay: time.Millisecond, PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	assertEventsJSONEqual(t, collectEvents(t, watcher.Events, 1, 2*time.Second), []SessionEvent{event})
	if watcher.Path() != path {
		t.Fatalf("watcher path = %q, want realpath project %q", watcher.Path(), path)
	}
}

func writeSessionEvents(t *testing.T, path string, events []SessionEvent, appendMode bool) {
	t.Helper()
	flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendMode {
		flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	file, err := os.OpenFile(path, flag, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
