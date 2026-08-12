package watch

import (
	"bytes"
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

func TestLiveClaudeWatcherWaitsForItsExactConversation(t *testing.T) {
	projectDir := t.TempDir()
	stateDir := t.TempDir()
	const liveID = "aaaaaaaa-1111-2222-3333-444444444444"
	const staleID = "bbbbbbbb-5555-6666-7777-888888888888"
	stale := SessionEvent{
		"type": "user", "uuid": "stale-event", "sessionId": staleID,
		"message": map[string]any{"role": "user", "content": "old folder history"},
	}
	writeSessionEvents(t, filepath.Join(projectDir, staleID+".jsonl"), []SessionEvent{stale}, false)
	mirrorPath := TranscriptMirrorPath(stateDir, liveID)

	watcher, err := WatchClaudeSession(ClaudeWatcherOptions{
		ClaudeSessionID: liveID,
		SessionID:       liveID,
		ProjectDir:      projectDir,
		MirrorPath:      mirrorPath,
		InitialDelay:    time.Millisecond,
		PollInterval:    10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()

	// The old transcript is the only file in the cwd bucket, but it belongs to
	// another provider conversation. A live runner must remain empty while it
	// waits for the exact file Claude was launched to create.
	assertNoEvent(t, watcher.Events, 80*time.Millisecond)
	if path := watcher.Path(); path != "" {
		t.Fatalf("watcher borrowed stale transcript %q", path)
	}

	livePath := filepath.Join(projectDir, liveID+".jsonl")
	live := SessionEvent{
		"type": "user", "uuid": "live-event", "sessionId": liveID,
		"message": map[string]any{"role": "user", "content": "new request"},
	}
	writeSessionEvents(t, livePath, []SessionEvent{live}, false)
	assertEventsJSONEqual(t, collectEvents(t, watcher.Events, 1, 2*time.Second), []SessionEvent{live})
	if watcher.Path() != livePath {
		t.Fatalf("watcher path = %q, want exact path %q", watcher.Path(), livePath)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		records, readErr := TranscriptMirrorRecords(mirrorPath)
		if readErr == nil && len(records) == 1 {
			mirrored := records[0]
			if mirrored["uuid"] != "live-event" {
				t.Fatalf("mirror contains %#v, want only live-event", mirrored)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mirror did not settle to one exact event: records=%d err=%v", len(records), readErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// sessionEventBytes renders events as the JSONL a provider would write, so a
// test can put them into a file by a route other than writeSessionEvents --
// notably an in-place WriteAt that preserves the inode.
func sessionEventBytes(t *testing.T, events []SessionEvent) []byte {
	t.Helper()
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	return buffer.Bytes()
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
