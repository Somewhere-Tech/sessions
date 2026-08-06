package watch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// conversationEvents is a small but realistic Claude exchange: a user turn and
// an assistant reply, each carrying the provider uuid the mirror keys on.
func conversationEvents(texts ...string) []SessionEvent {
	events := make([]SessionEvent, 0, len(texts))
	for i, text := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		events = append(events, SessionEvent{
			"type":      role,
			"uuid":      "uuid-" + text,
			"sessionId": "aaaaaaaa-1111-2222-3333-444444444444",
			"timestamp": "2026-08-05T10:0" + string(rune('0'+i%10)) + ":00Z",
			"message": map[string]any{
				"role":    role,
				"content": []any{map[string]any{"type": "text", "text": text}},
			},
		})
	}
	return events
}

func mirrorTexts(t *testing.T, mirrorPath string) []string {
	t.Helper()
	records, err := TranscriptMirrorRecords(mirrorPath)
	if err != nil {
		t.Fatalf("read mirror: %v", err)
	}
	texts := make([]string, 0, len(records))
	for _, record := range records {
		if text := eventText(record); text != "" {
			texts = append(texts, text)
		}
	}
	return texts
}

// TestMirrorProducesConversationAfterProviderFileDeleted is the case the whole
// design exists for. On this machine 81 of 93 conversations recorded in
// ~/.claude/history.jsonl no longer have a transcript, because the provider
// deletes them on a retention timer. Sessions must still be able to produce
// the conversation after that happens.
func TestMirrorProducesConversationAfterProviderFileDeleted(t *testing.T) {
	projectDir := t.TempDir()
	stateDir := t.TempDir()
	const sessionID = "sess-01"
	const providerID = "aaaaaaaa-1111-2222-3333-444444444444"
	providerPath := filepath.Join(projectDir, providerID+".jsonl")
	events := conversationEvents("what broke the deploy", "the migration ran twice")
	writeSessionEvents(t, providerPath, events, false)

	mirrorPath := TranscriptMirrorPath(stateDir, sessionID)
	watcher, err := WatchClaudeSession(ClaudeWatcherOptions{
		ClaudeSessionID: providerID,
		ProjectDir:      projectDir,
		SessionID:       sessionID,
		MirrorPath:      mirrorPath,
		InitialDelay:    time.Millisecond,
		PollInterval:    10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	collectEvents(t, watcher.Events, len(events), 2*time.Second)

	// The provider file goes away: retention cleanup, a user tidying ~/.claude,
	// or the project bucket being pruned.
	if err := os.Remove(providerPath); err != nil {
		t.Fatal(err)
	}
	watcher.Close()

	// Today's resolver, which is all Sessions has, now finds nothing at all.
	// This is the exact state that reports "Source kind: missing, Raw bytes 0".
	bare := ResolveClaudeJSONL(projectDir, providerID)
	if bare.Path != "" {
		t.Fatalf("provider resolution should be empty after deletion, got %q", bare.Path)
	}

	// With the mirror, the conversation is still resolvable and still readable.
	resolved := ResolveClaudeWithMirror(filepath.Dir(projectDir), projectDir, providerID, mirrorPath)
	if resolved.Path != mirrorPath || resolved.Reason != ClaudeMirror {
		t.Fatalf("resolution = %+v, want mirror path %q reason %q", resolved, mirrorPath, ClaudeMirror)
	}

	texts := mirrorTexts(t, mirrorPath)
	want := []string{"what broke the deploy", "the migration ran twice"}
	if strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Fatalf("conversation from mirror = %v, want %v", texts, want)
	}

	// The mirror is provider-shaped JSONL, so every existing reader works on it
	// by path substitution alone. Prove the stored bytes are the provider's.
	raw, err := os.ReadFile(mirrorPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var probe map[string]any
		if json.Unmarshal([]byte(line), &probe) != nil {
			t.Fatalf("mirror line is not valid provider JSONL: %q", line)
		}
		if probe["sessionId"] != providerID {
			t.Fatalf("mirror line lost provider sessionId: %q", line)
		}
	}

	meta, ok := ReadTranscriptMirrorMeta(mirrorPath)
	if !ok {
		t.Fatal("mirror sidecar missing")
	}
	if meta.Records != len(events) {
		t.Fatalf("meta.Records = %d, want %d", meta.Records, len(events))
	}
	// The sidecar remembers where the conversation came from even though that
	// path no longer exists — the only durable link back to the provider bucket.
	if meta.ProviderPath != providerPath {
		t.Fatalf("meta.ProviderPath = %q, want %q", meta.ProviderPath, providerPath)
	}
	if meta.ProviderSessionID != providerID {
		t.Fatalf("meta.ProviderSessionID = %q, want %q", meta.ProviderSessionID, providerID)
	}
}

// TestMirrorKeepsUnionWhenProviderRewritesSmaller covers provider compaction:
// Claude rewrites the transcript and drops earlier records. Sessions must keep
// what it already saw rather than following the provider down to the survivors.
func TestMirrorKeepsUnionWhenProviderRewritesSmaller(t *testing.T) {
	projectDir := t.TempDir()
	stateDir := t.TempDir()
	const providerID = "bbbbbbbb-1111-2222-3333-444444444444"
	providerPath := filepath.Join(projectDir, providerID+".jsonl")
	original := conversationEvents("first question", "first answer", "second question")
	writeSessionEvents(t, providerPath, original, false)

	mirrorPath := TranscriptMirrorPath(stateDir, "sess-02")
	watcher, err := WatchClaudeSession(ClaudeWatcherOptions{
		ClaudeSessionID: providerID, ProjectDir: projectDir,
		SessionID: "sess-02", MirrorPath: mirrorPath,
		InitialDelay: time.Millisecond, PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	collectEvents(t, watcher.Events, len(original), 2*time.Second)

	// Compaction: the provider keeps only the tail of the conversation. The
	// rewrite is strictly smaller, which is how the tail detects it.
	compacted := conversationEvents("second answer")
	writeSessionEvents(t, providerPath, compacted, false)
	collectEvents(t, watcher.Events, 1, 2*time.Second)
	watcher.Close()

	texts := mirrorTexts(t, mirrorPath)
	want := []string{"first question", "first answer", "second question", "second answer"}
	if strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Fatalf("mirror after compaction = %v, want union %v", texts, want)
	}

	meta, _ := ReadTranscriptMirrorMeta(mirrorPath)
	if meta.Generations < 1 {
		t.Fatalf("meta.Generations = %d, want at least 1 recorded provider rewrite", meta.Generations)
	}
	if meta.Records != len(want) {
		t.Fatalf("meta.Records = %d, want %d", meta.Records, len(want))
	}
}

// TestMirrorBackfillsConversationThatStartedBeforeWatching is the adoption
// case: a conversation already exists when Sessions begins watching it. The
// watcher reads from offset zero on attach, so the mirror captures the history
// rather than only what happens from now on.
func TestMirrorBackfillsConversationThatStartedBeforeWatching(t *testing.T) {
	projectDir := t.TempDir()
	stateDir := t.TempDir()
	const providerID = "cccccccc-1111-2222-3333-444444444444"
	providerPath := filepath.Join(projectDir, providerID+".jsonl")
	existing := conversationEvents("older turn one", "older turn two", "older turn three")
	writeSessionEvents(t, providerPath, existing, false)

	mirrorPath := TranscriptMirrorPath(stateDir, "sess-03")
	watcher, err := WatchClaudeSession(ClaudeWatcherOptions{
		ClaudeSessionID: providerID, ProjectDir: projectDir,
		SessionID: "sess-03", MirrorPath: mirrorPath,
		InitialDelay: time.Millisecond, PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	collectEvents(t, watcher.Events, len(existing), 2*time.Second)
	watcher.Close()

	if got := len(mirrorTexts(t, mirrorPath)); got != len(existing) {
		t.Fatalf("mirrored %d pre-existing records, want %d", got, len(existing))
	}
}

// TestMirrorAppendIsIdempotentAcrossReopen simulates a daemon restart. The
// watcher always re-reads the provider file from offset zero, so without a
// durable identity set every restart would append the whole conversation again.
func TestMirrorAppendIsIdempotentAcrossReopen(t *testing.T) {
	stateDir := t.TempDir()
	mirrorPath := TranscriptMirrorPath(stateDir, "sess-04")
	lines := [][]byte{
		[]byte(`{"uuid":"a","type":"user","message":{"content":"one"}}`),
		[]byte(`{"uuid":"b","type":"assistant","message":{"content":"two"}}`),
	}

	first, err := OpenTranscriptMirror(TranscriptMirrorOptions{Path: mirrorPath, SessionID: "sess-04"})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		if appended, err := first.Append(line); err != nil || !appended {
			t.Fatalf("first append(%s) = %v, %v", line, appended, err)
		}
	}
	sizeAfterFirst := first.Meta().Bytes
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart: same records replayed, plus one genuinely new record.
	second, err := OpenTranscriptMirror(TranscriptMirrorOptions{Path: mirrorPath, SessionID: "sess-04"})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if got := second.Meta().Records; got != len(lines) {
		t.Fatalf("reopened mirror knows %d records, want %d", got, len(lines))
	}
	for _, line := range lines {
		if appended, err := second.Append(line); err != nil || appended {
			t.Fatalf("replayed append(%s) = %v, %v; want no write", line, appended, err)
		}
	}
	if got := second.Meta().Bytes; got != sizeAfterFirst {
		t.Fatalf("mirror grew on replay: %d -> %d", sizeAfterFirst, got)
	}
	fresh := []byte(`{"uuid":"c","type":"user","message":{"content":"three"}}`)
	if appended, err := second.Append(fresh); err != nil || !appended {
		t.Fatalf("new append = %v, %v; want a write", appended, err)
	}
	if got := second.Meta().Records; got != 3 {
		t.Fatalf("records = %d, want 3", got)
	}
}

// TestMirrorDeduplicatesRecordsWithoutUUID covers provider lines that carry no
// uuid, such as summary records. A content hash keeps them idempotent too.
func TestMirrorDeduplicatesRecordsWithoutUUID(t *testing.T) {
	mirrorPath := TranscriptMirrorPath(t.TempDir(), "sess-05")
	mirror, err := OpenTranscriptMirror(TranscriptMirrorOptions{Path: mirrorPath})
	if err != nil {
		t.Fatal(err)
	}
	defer mirror.Close()
	line := []byte(`{"type":"summary","summary":"fixed the deploy"}`)
	if appended, _ := mirror.Append(line); !appended {
		t.Fatal("first append of uuid-less record should write")
	}
	if appended, _ := mirror.Append(line); appended {
		t.Fatal("repeated uuid-less record should be deduplicated by content")
	}
	if got := mirror.Meta().Records; got != 1 {
		t.Fatalf("records = %d, want 1", got)
	}
}

// TestResolveWithMirrorPrefersProviderFile is the no-double-counting
// invariant. While the provider file exists it stays authoritative, so a
// session resolves to exactly one transcript and search and usage cannot see
// the same conversation twice.
func TestResolveWithMirrorPrefersProviderFile(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "work")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	projects := filepath.Join(home, ".claude", "projects")
	projectDir := filepath.Join(projects, EncodeClaudeCWD(cwd))
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const providerID = "dddddddd-1111-2222-3333-444444444444"
	providerPath := filepath.Join(projectDir, providerID+".jsonl")
	writeSessionEvents(t, providerPath, conversationEvents("live turn"), false)

	mirrorPath := TranscriptMirrorPath(t.TempDir(), "sess-06")
	mirror, err := OpenTranscriptMirror(TranscriptMirrorOptions{Path: mirrorPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mirror.Append([]byte(`{"uuid":"x","type":"user"}`)); err != nil {
		t.Fatal(err)
	}
	mirror.Close()

	resolved := ResolveClaudeWithMirror(projects, cwd, providerID, mirrorPath)
	if resolved.Path != providerPath || resolved.Reason != ClaudeExact {
		t.Fatalf("resolution = %+v, want provider %q reason %q", resolved, providerPath, ClaudeExact)
	}

	// An empty mirror is not a usable fallback either.
	empty := TranscriptMirrorPath(t.TempDir(), "sess-07")
	if _, err := os.Create(empty); err != nil {
		t.Fatal(err)
	}
	if TranscriptMirrorUsable(empty) {
		t.Fatal("an empty mirror must not be offered as a conversation")
	}
	missing := ResolveClaudeWithMirror(projects, filepath.Join(home, "elsewhere"), providerID, empty)
	if missing.Path != "" {
		t.Fatalf("resolution with empty mirror = %+v, want no path", missing)
	}
}

// TestMirrorCapStopsAppendingWithoutDiscarding proves the retention policy:
// reaching the cap stops new writes and is recorded, and nothing already
// stored is truncated or rotated away.
func TestMirrorCapStopsAppendingWithoutDiscarding(t *testing.T) {
	mirrorPath := TranscriptMirrorPath(t.TempDir(), "sess-08")
	first := []byte(`{"uuid":"a","type":"user","message":{"content":"kept"}}`)
	mirror, err := OpenTranscriptMirror(TranscriptMirrorOptions{
		Path: mirrorPath, CapBytes: int64(len(first)) + 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mirror.Close()
	if appended, _ := mirror.Append(first); !appended {
		t.Fatal("first record should fit the cap")
	}
	if appended, _ := mirror.Append([]byte(`{"uuid":"b","type":"user","message":{"content":"dropped"}}`)); appended {
		t.Fatal("append past the cap should not write")
	}
	meta := mirror.Meta()
	if !meta.Capped {
		t.Fatal("meta.Capped should record that the cap was reached")
	}
	if meta.Records != 1 {
		t.Fatalf("records = %d, want the already-stored record retained", meta.Records)
	}
	if got := mirrorTexts(t, mirrorPath); len(got) != 0 && got[0] != "kept" {
		t.Fatalf("stored record was disturbed by the cap: %v", got)
	}
}

// TestMirrorSurvivesAmbiguousProjectBucket covers the second loss mode, which
// needs no deletion at all. Claude's project-directory encoding replaces path
// separators with dashes, so /Users/uzair/pretty-PTY-desktop-ux and
// /Users/uzair/pretty-PTY/desktop-ux collapse into one bucket — both of those
// encodings exist on this machine today. Once a bucket holds more than one
// transcript and the launch UUID is not known exactly, the resolver correctly
// refuses to guess and returns no path, and the conversation disappears from
// cat, source, and search even though the file is still on disk.
func TestMirrorSurvivesAmbiguousProjectBucket(t *testing.T) {
	projectDir := t.TempDir()
	stateDir := t.TempDir()
	const providerID = "ffffffff-1111-2222-3333-444444444444"
	providerPath := filepath.Join(projectDir, providerID+".jsonl")
	writeSessionEvents(t, providerPath, conversationEvents("the work that matters"), false)

	mirrorPath := TranscriptMirrorPath(stateDir, "sess-09")
	watcher, err := WatchClaudeSession(ClaudeWatcherOptions{
		ClaudeSessionID: providerID, ProjectDir: projectDir,
		SessionID: "sess-09", MirrorPath: mirrorPath,
		InitialDelay: time.Millisecond, PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	collectEvents(t, watcher.Events, 1, 2*time.Second)
	watcher.Close()

	// A second conversation lands in the same collapsed bucket.
	writeSessionEvents(t, filepath.Join(projectDir, "99999999-1111-2222-3333-444444444444.jsonl"),
		conversationEvents("an unrelated conversation"), false)

	// Without an exact launch UUID the bucket is now ambiguous and yields no
	// path, which is what makes the session read as missing.
	if bare := ResolveClaudeJSONL(projectDir, ""); bare.Path != "" || bare.Reason != ClaudeAmbiguous {
		t.Fatalf("expected ambiguous resolution, got %+v", bare)
	}
	resolved := ResolveClaudeWithMirror(filepath.Dir(projectDir), projectDir, "", mirrorPath)
	if resolved.Path != mirrorPath || resolved.Reason != ClaudeMirror {
		t.Fatalf("resolution = %+v, want mirror fallback", resolved)
	}
	if got := mirrorTexts(t, mirrorPath); len(got) != 1 || got[0] != "the work that matters" {
		t.Fatalf("mirror conversation = %v, want the session's own turn", got)
	}
}

// TestMirrorDisabledByDefault keeps the change additive: without a MirrorPath
// the watcher behaves exactly as before and writes nothing.
func TestMirrorDisabledByDefault(t *testing.T) {
	projectDir := t.TempDir()
	stateDir := t.TempDir()
	const providerID = "eeeeeeee-1111-2222-3333-444444444444"
	writeSessionEvents(t, filepath.Join(projectDir, providerID+".jsonl"), conversationEvents("only turn"), false)

	watcher, err := WatchClaudeSession(ClaudeWatcherOptions{
		ClaudeSessionID: providerID, ProjectDir: projectDir,
		InitialDelay: time.Millisecond, PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	collectEvents(t, watcher.Events, 1, 2*time.Second)
	watcher.Close()

	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unmirrored watcher wrote %d files into the state dir", len(entries))
	}
}
