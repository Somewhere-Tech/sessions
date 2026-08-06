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

// TestMirrorKeepsRepeatedRecordsWithoutUUID pins the multiset rule for provider
// lines that carry no uuid.
//
// This previously asserted the opposite -- that a repeated uuid-less line was
// deduplicated by content -- and that was a data-loss bug rather than an
// optimisation. Real Claude transcripts repeat these lines constantly: of 6607
// uuid-less records in ~/.claude/projects on the development machine, 3879 were
// byte-identical to an earlier one, almost all of them "mode",
// "permission-mode", "agent-name" and "custom-title" state records. Those are
// read back last-one-wins by native `claude --resume` to restore model,
// permission mode, and agent. Collapsing them rewinds the restored state: a
// conversation that toggled bypassPermissions -> normal -> bypassPermissions
// would resume in normal mode.
//
// Within one replay pass every occurrence is therefore stored.
func TestMirrorKeepsRepeatedRecordsWithoutUUID(t *testing.T) {
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
	if appended, _ := mirror.Append(line); !appended {
		t.Fatal("second occurrence of a uuid-less record is a distinct record and must be kept")
	}
	if got := mirror.Meta().Records; got != 2 {
		t.Fatalf("records = %d, want 2", got)
	}
}

// TestMirrorRepeatedRecordsAreIdempotentAcrossPasses is the other half of the
// rule. Keeping repeats would be worthless if a re-read of the same provider
// file appended them all again on every attach, so a fresh pass renumbers
// occurrences from the start and stores nothing it already holds.
func TestMirrorRepeatedRecordsAreIdempotentAcrossPasses(t *testing.T) {
	mirrorPath := TranscriptMirrorPath(t.TempDir(), "sess-passes")
	mirror, err := OpenTranscriptMirror(TranscriptMirrorOptions{Path: mirrorPath})
	if err != nil {
		t.Fatal(err)
	}
	defer mirror.Close()

	// A realistic uuid-less state sequence: the same value returns after a
	// different one, so it is a repeat that must not collapse.
	pass := [][]byte{
		[]byte(`{"type":"permission-mode","permissionMode":"bypassPermissions","sessionId":"s"}`),
		[]byte(`{"type":"permission-mode","permissionMode":"normal","sessionId":"s"}`),
		[]byte(`{"type":"permission-mode","permissionMode":"bypassPermissions","sessionId":"s"}`),
	}
	replay := func() int {
		mirror.BeginPass()
		written := 0
		for _, line := range pass {
			appended, appendErr := mirror.Append(line)
			if appendErr != nil {
				t.Fatal(appendErr)
			}
			if appended {
				written++
			}
		}
		return written
	}

	if got := replay(); got != 3 {
		t.Fatalf("first pass wrote %d records, want 3", got)
	}
	if got := replay(); got != 0 {
		t.Fatalf("replaying an unchanged pass wrote %d records, want 0", got)
	}
	if got := mirror.Meta().Records; got != 3 {
		t.Fatalf("records = %d, want 3", got)
	}

	// The last record must still be bypassPermissions. That is the property a
	// resume depends on, so assert the restored value rather than only a count.
	if err := mirror.Sync(); err != nil {
		t.Fatal(err)
	}
	records, err := TranscriptMirrorRecords(mirrorPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("mirror holds %d records, want 3", len(records))
	}
	if got := records[len(records)-1]["permissionMode"]; got != "bypassPermissions" {
		t.Fatalf("last permission mode = %v, want bypassPermissions", got)
	}
}

// TestMirrorReopenPreservesRepeatOrdinals covers the daemon-restart path. The
// occurrence numbering has to be rebuilt from the mirror file itself, or the
// first pass after a restart would treat every repeat as new and duplicate the
// whole conversation's state records.
func TestMirrorReopenPreservesRepeatOrdinals(t *testing.T) {
	dir := t.TempDir()
	mirrorPath := TranscriptMirrorPath(dir, "sess-reopen")
	line := []byte(`{"type":"mode","mode":"normal","sessionId":"s"}`)

	first, err := OpenTranscriptMirror(TranscriptMirrorOptions{Path: mirrorPath})
	if err != nil {
		t.Fatal(err)
	}
	first.BeginPass()
	for range 3 {
		if _, err := first.Append(line); err != nil {
			t.Fatal(err)
		}
	}
	if got := first.Meta().Records; got != 3 {
		t.Fatalf("records before reopen = %d, want 3", got)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := OpenTranscriptMirror(TranscriptMirrorOptions{Path: mirrorPath})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if got := second.Meta().Records; got != 3 {
		t.Fatalf("records after reopen = %d, want 3", got)
	}
	second.BeginPass()
	for range 3 {
		if appended, err := second.Append(line); err != nil || appended {
			t.Fatalf("replay after reopen wrote a duplicate: appended=%v err=%v", appended, err)
		}
	}
	if got := second.Meta().Records; got != 3 {
		t.Fatalf("records after replay = %d, want 3", got)
	}
	// A fourth genuine occurrence still lands.
	if appended, err := second.Append(line); err != nil || !appended {
		t.Fatalf("fourth occurrence = %v, %v; want a write", appended, err)
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

	// The sidecar recorded the exact provider file the watcher observed, so the
	// ambiguity is now settled by evidence and the LIVE provider file wins. That
	// is strictly better than the mirror: the mirror is a snapshot, and serving
	// it to a session that is still running would freeze its conversation.
	resolved := ResolveClaudeWithMirror(filepath.Dir(projectDir), projectDir, "", mirrorPath)
	if resolved.Path != providerPath || resolved.Reason != ClaudeSidecarPath {
		t.Fatalf("resolution = %+v, want the recorded provider path %q", resolved, providerPath)
	}
	if got := mirrorTexts(t, mirrorPath); len(got) != 1 || got[0] != "the work that matters" {
		t.Fatalf("mirror conversation = %v, want the session's own turn", got)
	}

	// Once the provider file is gone the recorded path is no longer evidence of
	// anything, and the mirror is the last copy. This is the ordering that keeps
	// a conversation resolving to exactly one transcript at a time.
	if err := os.Remove(providerPath); err != nil {
		t.Fatal(err)
	}
	fallback := ResolveClaudeWithMirror(filepath.Dir(projectDir), projectDir, "", mirrorPath)
	if fallback.Path != mirrorPath || fallback.Reason != ClaudeMirror {
		t.Fatalf("resolution after provider deletion = %+v, want mirror fallback", fallback)
	}
}

// TestResolveWithMirrorRecoversRenamedBucketByProviderID covers the other half
// of the sidecar evidence. A renamed working directory orphans the bucket
// Sessions writes into, so the recorded provider path stops existing while the
// conversation is still on disk under a bucket the encoder no longer produces.
// The recorded provider id still names the file exactly.
func TestResolveWithMirrorRecoversRenamedBucketByProviderID(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, ".claude", "projects")
	cwd := filepath.Join(root, "work")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	bucket := filepath.Join(projects, EncodeClaudeCWD(cwd))
	if err := os.MkdirAll(bucket, 0o755); err != nil {
		t.Fatal(err)
	}

	const providerID = "abcdef00-1111-2222-3333-444444444444"
	providerPath := filepath.Join(bucket, providerID+".jsonl")
	writeSessionEvents(t, providerPath, conversationEvents("still here"), false)
	// A second transcript makes the bucket ambiguous without an exact id.
	writeSessionEvents(t, filepath.Join(bucket, "11111111-1111-2222-3333-444444444444.jsonl"),
		conversationEvents("someone else"), false)

	mirrorPath := TranscriptMirrorPath(t.TempDir(), "sess-renamed")
	mirror, err := OpenTranscriptMirror(TranscriptMirrorOptions{
		Path: mirrorPath, SessionID: "sess-renamed", ProviderSessionID: providerID, Tool: "claude",
	})
	if err != nil {
		t.Fatal(err)
	}
	// A path that no longer exists, as a rename or a machine move would leave.
	mirror.NoteProviderPath(filepath.Join(projects, "-gone", providerID+".jsonl"))
	if _, err := mirror.Append([]byte(`{"uuid":"x","type":"user"}`)); err != nil {
		t.Fatal(err)
	}
	if err := mirror.Close(); err != nil {
		t.Fatal(err)
	}

	resolved := ResolveClaudeWithMirror(projects, cwd, "", mirrorPath)
	if resolved.Path != providerPath || resolved.Reason != ClaudeSidecarID {
		t.Fatalf("resolution = %+v, want %q via the recorded provider id", resolved, providerPath)
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
