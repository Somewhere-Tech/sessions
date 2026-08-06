package integrations

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

// A Codex rollout Sessions did not create is discovered through the provider
// scan, and the surface it was started from has to survive the trip into a
// history row. This is the case the whole feature exists for: the user's
// Desktop conversations are indistinguishable from their CLI ones in Codex's
// own picker.
func TestHistoryCarriesCodexDesktopProvenanceForADiscoveredConversation(t *testing.T) {
	root := t.TempDir()
	runnerDir := filepath.Join(root, "runners")
	sessionsDir := filepath.Join(root, "codex-sessions")
	claudeDir := filepath.Join(root, "claude-projects")
	cwd := filepath.Join(root, "pretty-PTY")
	for _, dir := range []string{runnerDir, sessionsDir, claudeDir, cwd} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	rolloutPath := filepath.Join(sessionsDir, "2026", "08", "05",
		"rollout-2026-08-05T13-33-15-019fd3a1.jsonl")
	if err := os.MkdirAll(filepath.Dir(rolloutPath), 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSONL(t, rolloutPath, []map[string]any{
		{"timestamp": "2026-08-05T13:33:15Z", "type": "session_meta", "payload": map[string]any{
			"id": "019fd3a1-5dcf-7850-8b19-12c6a6ce3f2a", "cwd": cwd,
			"timestamp": "2026-08-05T13:33:15Z", "originator": "Codex Desktop",
			"source": "vscode", "thread_source": "automation", "cli_version": "0.104.0",
		}},
		{"timestamp": "2026-08-05T13:34:00Z", "type": "response_item", "payload": map[string]any{
			"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": "nightly sweep"}},
		}},
	})

	store := NewHistoryStore(HistoryOptions{
		RunnerStateDir: runnerDir, ClaudeProjectsDir: claudeDir, CodexSessionsDir: sessionsDir,
		Machine: "fixture", DiscoverProviderHistory: true,
	})
	listing, err := store.List(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Sessions) != 1 {
		t.Fatalf("sessions = %#v", listing.Sessions)
	}
	session := listing.Sessions[0]
	if session.Surface == nil {
		t.Fatal("a Codex Desktop conversation reported no surface")
	}
	if session.Surface.Kind != watch.SurfaceCodexDesktop ||
		session.Surface.Label != "Codex Desktop" ||
		session.Surface.Originator != "Codex Desktop" ||
		session.Surface.Source != "vscode" ||
		session.Surface.Actor != watch.ActorAutomation ||
		session.Surface.ActorRaw != "automation" {
		t.Fatalf("surface = %#v", session.Surface)
	}

	// Additive on the wire: a decoder that does not know the field is
	// unaffected, and one that does sees the raw values alongside the label.
	encoded, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"surface"`, `"kind":"codex-desktop"`, `"originator":"Codex Desktop"`, `"actor":"automation"`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("history JSON missing %s: %s", want, encoded)
		}
	}
}

// A managed Sessions record reads its provenance from the resolved provider
// file, which is the only place it exists.
func TestHistoryCarriesProvenanceForASessionsManagedConversation(t *testing.T) {
	root := t.TempDir()
	runnerDir := filepath.Join(root, "runners")
	sessionsDir := filepath.Join(root, "codex-sessions")
	claudeDir := filepath.Join(root, "claude-projects")
	cwd := filepath.Join(root, "worktree")
	for _, dir := range []string{runnerDir, sessionsDir, claudeDir, cwd} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	id := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	resumeID := "01234567-89ab-4cde-8fab-0123456789ab"
	created := time.Date(2026, time.August, 5, 13, 0, 0, 0, time.UTC)
	if err := state.WriteMetadata(filepath.Join(runnerDir, id+".json"), state.Metadata{
		ID: id, Name: "sessions-started codex", Cmd: "codex", Args: []string{"resume", resumeID},
		Cwd: cwd, CreatedAt: created.UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	rolloutPath := filepath.Join(sessionsDir, "2026", "08", "05", "rollout-"+resumeID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(rolloutPath), 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSONL(t, rolloutPath, []map[string]any{
		{"timestamp": "2026-08-05T13:01:00Z", "type": "session_meta", "payload": map[string]any{
			"id": resumeID, "cwd": cwd, "timestamp": "2026-08-05T13:01:00Z",
			"originator": "pretty-pty", "source": "vscode",
		}},
		{"timestamp": "2026-08-05T13:02:00Z", "type": "response_item", "payload": map[string]any{
			"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": "hello"}},
		}},
	})

	store := NewHistoryStore(HistoryOptions{
		RunnerStateDir: runnerDir, ClaudeProjectsDir: claudeDir, CodexSessionsDir: sessionsDir,
		Machine: "fixture",
	})
	listing, err := store.List(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Sessions) != 1 {
		t.Fatalf("sessions = %#v", listing.Sessions)
	}
	surface := listing.Sessions[0].Surface
	if surface == nil || surface.Kind != watch.SurfaceSessions || surface.Label != "Codex via Sessions" {
		t.Fatalf("surface = %#v", surface)
	}
	// Sessions' own runner still identifies itself by its old name, and the raw
	// value is kept so that stays checkable.
	if surface.Originator != "pretty-pty" {
		t.Fatalf("raw originator lost: %#v", surface)
	}
	// Nothing recorded who drove it, and nothing may be invented.
	if surface.Actor != "" {
		t.Fatalf("actor invented: %#v", surface)
	}
}

// Ordering a conversation browser by file modification time is ordering it by
// an artefact of the last copy. A plain `cp -R` of this machine's history made
// all 203 conversations report the same instant; the conversations themselves
// carry the durable answer.
func TestHistoryDatesConversationsByTheirOwnRecords(t *testing.T) {
	root := t.TempDir()
	runnerDir := filepath.Join(root, "runners")
	sessionsDir := filepath.Join(root, "codex-sessions")
	claudeDir := filepath.Join(root, "claude-projects")
	for _, dir := range []string{runnerDir, sessionsDir, claudeDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	older := filepath.Join(claudeDir, "-fixture-one")
	newer := filepath.Join(claudeDir, "-fixture-two")
	for _, dir := range []string{older, newer} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeClaudeConversation(t, filepath.Join(older, "11111111-1111-4111-8111-111111111111.jsonl"),
		"2026-07-01T09:00:00Z", "the older conversation")
	writeClaudeConversation(t, filepath.Join(newer, "22222222-2222-4222-8222-222222222222.jsonl"),
		"2026-08-05T13:00:00Z", "the newer conversation")

	// Both files are stamped with the same instant, as a copy would leave them,
	// and the older conversation is given the later mtime so that mtime and the
	// records disagree about the ordering.
	copied := time.Date(2026, time.December, 25, 9, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(older, "11111111-1111-4111-8111-111111111111.jsonl"),
		copied.Add(time.Minute), copied.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(newer, "22222222-2222-4222-8222-222222222222.jsonl"),
		copied, copied); err != nil {
		t.Fatal(err)
	}

	store := NewHistoryStore(HistoryOptions{
		RunnerStateDir: runnerDir, ClaudeProjectsDir: claudeDir, CodexSessionsDir: sessionsDir,
		Machine: "fixture", DiscoverProviderHistory: true,
	})
	listing, err := store.List(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Sessions) != 2 {
		t.Fatalf("sessions = %#v", listing.Sessions)
	}
	byName := map[string]HistorySession{}
	for _, session := range listing.Sessions {
		byName[session.Name] = session
	}
	oldest, newest := byName["the older conversation"], byName["the newer conversation"]
	if oldest.ConversationUpdatedApproximate || newest.ConversationUpdatedApproximate {
		t.Fatalf("timestamps not taken from records: %#v %#v", oldest, newest)
	}
	wantOld := time.Date(2026, time.July, 1, 9, 0, 0, 0, time.UTC).UnixMilli()
	wantNew := time.Date(2026, time.August, 5, 13, 0, 0, 0, time.UTC).UnixMilli()
	if oldest.ConversationUpdatedAt != wantOld || newest.ConversationUpdatedAt != wantNew {
		t.Fatalf("updated: old=%d want %d, new=%d want %d",
			oldest.ConversationUpdatedAt, wantOld, newest.ConversationUpdatedAt, wantNew)
	}
	if oldest.LastActivityAt >= newest.LastActivityAt {
		t.Fatalf("the copy's modification times still decide the order: old=%d new=%d",
			oldest.LastActivityAt, newest.LastActivityAt)
	}
}

// A transcript that stamped nothing still gets a time, and says that it is the
// file's rather than the conversation's.
func TestHistoryFallsBackToFileTimeAndSaysSo(t *testing.T) {
	root := t.TempDir()
	runnerDir := filepath.Join(root, "runners")
	claudeDir := filepath.Join(root, "claude-projects")
	bucket := filepath.Join(claudeDir, "-fixture-bridge")
	for _, dir := range []string{runnerDir, bucket} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(bucket, "33333333-3333-4333-8333-333333333333.jsonl")
	contents := `{"type":"user","message":{"role":"user","content":"no timestamps here"}}` + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	modified := time.Date(2026, time.August, 5, 21, 42, 0, 0, time.UTC)
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}

	store := NewHistoryStore(HistoryOptions{
		RunnerStateDir: runnerDir, ClaudeProjectsDir: claudeDir,
		CodexSessionsDir: filepath.Join(root, "absent"),
		Machine:          "fixture", DiscoverProviderHistory: true,
	})
	listing, err := store.List(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Sessions) != 1 {
		t.Fatalf("sessions = %#v", listing.Sessions)
	}
	session := listing.Sessions[0]
	if session.ConversationUpdatedAt != modified.UnixMilli() {
		t.Fatalf("updated = %d, want the file time %d", session.ConversationUpdatedAt, modified.UnixMilli())
	}
	if !session.ConversationUpdatedApproximate {
		t.Fatal("a file-time answer claimed to come from the conversation's own records")
	}
}

func writeJSONL(t *testing.T, path string, records []map[string]any) {
	t.Helper()
	var builder strings.Builder
	for _, record := range records {
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		builder.Write(encoded)
		builder.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(builder.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeClaudeConversation(t *testing.T, path, timestamp, text string) {
	t.Helper()
	writeJSONL(t, path, []map[string]any{{
		"type": "user", "timestamp": timestamp, "entrypoint": "cli",
		"cwd": filepath.Dir(path), "promptSource": "typed",
		"message": map[string]any{"role": "user", "content": text},
	}})
}

// The cheap listing must stay cheap and stop lying. Counting a transcript means
// parsing all of it, which is seven seconds cold over this machine's real 303
// conversations, so the summary view declines to parse — but it already knows
// the answer for every transcript counted since the daemon started, and
// reporting those as zero is what forced the app's conversation browser onto
// the expensive listing on first paint.
func TestSummaryListingReportsCachedCountsAndAdmitsTheRest(t *testing.T) {
	root := t.TempDir()
	runnerDir := filepath.Join(root, "runners")
	claudeDir := filepath.Join(root, "claude-projects")
	counted := filepath.Join(claudeDir, "-fixture-counted")
	uncounted := filepath.Join(claudeDir, "-fixture-uncounted")
	for _, dir := range []string{runnerDir, counted, uncounted} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	countedPath := filepath.Join(counted, "11111111-1111-4111-8111-111111111111.jsonl")
	writeClaudeConversation(t, countedPath, "2026-08-05T13:00:00Z", "counted conversation")
	writeClaudeConversation(t, filepath.Join(uncounted, "22222222-2222-4222-8222-222222222222.jsonl"),
		"2026-08-05T12:00:00Z", "uncounted conversation")

	store := NewHistoryStore(HistoryOptions{
		RunnerStateDir: runnerDir, ClaudeProjectsDir: claudeDir,
		CodexSessionsDir: filepath.Join(root, "absent"),
		Machine:          "fixture", DiscoverProviderHistory: true,
	})

	// Nothing has been counted yet, so the cheap view answers for nothing and
	// says so rather than reporting two empty conversations.
	sessions, err := store.SearchSessions(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %#v", sessions)
	}
	for _, session := range sessions {
		if !session.MessageCountUncounted || session.MessageCount != 0 {
			t.Fatalf("cold summary claimed a count: %#v", session)
		}
	}

	// Counting one transcript through any path fills the cache.
	if _, err := store.Transcript(nil, externalHistoryID("claude", "11111111-1111-4111-8111-111111111111")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.messageCount(countedPath, "claude", statOf(t, countedPath)); err != nil {
		t.Fatal(err)
	}

	sessions, err = store.SearchSessions(nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]HistorySession{}
	for _, session := range sessions {
		byName[session.Name] = session
	}
	warm := byName["counted conversation"]
	if warm.MessageCountUncounted || warm.MessageCount != 1 {
		t.Fatalf("a cached count was not reported: %#v", warm)
	}
	cold := byName["uncounted conversation"]
	if !cold.MessageCountUncounted {
		t.Fatalf("an uncounted row reported a count: %#v", cold)
	}

	// The exact listing still counts everything, and never marks a row.
	listing, err := store.List(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range listing.Sessions {
		if session.MessageCountUncounted || session.MessageCount != 1 {
			t.Fatalf("the exact listing degraded: %#v", session)
		}
	}
}

func statOf(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}
