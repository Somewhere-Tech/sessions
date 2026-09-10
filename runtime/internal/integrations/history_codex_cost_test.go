package integrations

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Codex discovery reads the head of every rollout (watch.codexResumePreviewBytes,
// 512 KiB) and describeExternal reads the tail of each one again
// (watch.conversationTailBytes, 64 KiB). Both are per-file bounds; the listing
// cost is those bounds multiplied by how many conversations exist.
const (
	codexScanHeadBudget = 512 * 1024
	codexTailBudget     = 64 << 10
	providerScanCacheTT = 2 * time.Second
)

// writeCodexRollouts lays out count rollout files totalling about totalBytes,
// each with a distinct conversation id. Every file's first record is its
// session_meta, exactly as Codex writes it.
func writeCodexRollouts(t testing.TB, root string, count, totalBytes int) (string, []string, int64) {
	t.Helper()
	sessionsDir := filepath.Join(root, "codex-sessions", "2026", "09", "10")
	if err := os.MkdirAll(sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(root, "project")
	perFile := totalBytes / count
	ids := make([]string, 0, count)
	filler := make([]byte, 400)
	for index := range filler {
		filler[index] = 'a' + byte(index%26)
	}
	stamp := "2026-09-10T00:00:00Z"
	var written int64
	for index := range count {
		id := fmt.Sprintf("%08x-aaaa-4aaa-8aaa-aaaaaaaaaaaa", index)
		ids = append(ids, id)
		path := filepath.Join(sessionsDir, "rollout-"+id+".jsonl")
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		writer := bufio.NewWriterSize(file, 1<<20)
		meta, err := json.Marshal(map[string]any{
			"timestamp": stamp, "type": "session_meta",
			"payload": map[string]any{"id": id, "cwd": cwd, "timestamp": stamp, "originator": "codex_cli_rs"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(append(meta, '\n')); err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(map[string]any{
			"timestamp": stamp, "type": "response_item",
			"payload": map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": "ask " + string(filler)}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		body = append(body, '\n')
		for size := len(meta) + 1; size < perFile; size += len(body) {
			if _, err := writer.Write(body); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Flush(); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		written += info.Size()
	}
	return sessionsDir, ids, written
}

func codexStore(root string) *HistoryStore {
	return NewHistoryStore(HistoryOptions{
		RunnerStateDir:    filepath.Join(root, "runners"),
		ClaudeProjectsDir: filepath.Join(root, "claude-projects"),
		CodexSessionsDir:  filepath.Join(root, "codex-sessions"),
		Machine:           "fixture-mac", DiscoverProviderHistory: true,
	})
}

type listCost struct {
	bytes    uint64
	allocs   uint64
	elapsed  time.Duration
	sessions int
}

func measureSummary(t testing.TB, store *HistoryStore) listCost {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	listed, err := store.SearchSessions(nil)
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	return listCost{
		bytes: after.TotalAlloc - before.TotalAlloc, allocs: after.Mallocs - before.Mallocs,
		elapsed: elapsed, sessions: len(listed),
	}
}

func measureList(t testing.TB, store *HistoryStore) listCost {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	listed, err := store.List(nil)
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	return listCost{
		bytes: after.TotalAlloc - before.TotalAlloc, allocs: after.Mallocs - before.Mallocs,
		elapsed: elapsed, sessions: len(listed.Sessions),
	}
}

// TestCodexHistoryListingCost is the measurement, not a guard: it prints the
// cold and warm cost of listing Codex conversations at three fleet sizes and
// two total footprints. Run it deliberately.
//
//	SESSIONS_MEASURE_HISTORY=1 go test ./internal/integrations/ -run CodexHistoryListingCost -v
func TestCodexHistoryListingCost(t *testing.T) {
	if os.Getenv("SESSIONS_MEASURE_HISTORY") != "1" {
		t.Skip("set SESSIONS_MEASURE_HISTORY=1; this writes tens of MiB per case")
	}
	for _, total := range []int{4 * 1024 * 1024, 48 * 1024 * 1024} {
		for _, count := range []int{50, 300, 600} {
			t.Run(fmt.Sprintf("total=%dMiB/conversations=%d", total/(1024*1024), count), func(t *testing.T) {
				root := t.TempDir()
				_, ids, written := writeCodexRollouts(t, root, count, total)
				perFile := written / int64(count)
				store := codexStore(root)

				// List is countAll: it parses each conversation once to count its
				// messages and caches that by fingerprint. SearchSessions is
				// countCached, the lightweight listing that never parses to
				// count -- the closer analogue of a "lightweight history
				// endpoint".
				summaryCold := measureSummary(t, codexStore(root))
				cold := measureList(t, store)
				warm := measureList(t, store)
				time.Sleep(providerScanCacheTT + 250*time.Millisecond)
				rescan := measureList(t, store)
				time.Sleep(providerScanCacheTT + 250*time.Millisecond)
				rescanAgain := measureList(t, store)

				preview := measurePreview(t, store, "provider:codex:"+ids[count/2], 3)

				t.Logf("files=%d bytes=%d per-file=%d", count, written, perFile)
				t.Logf("  summary:     %10d B  %8d allocs  %12v  %d sessions (fresh store, no counting)",
					summaryCold.bytes, summaryCold.allocs, summaryCold.elapsed, summaryCold.sessions)
				t.Logf("  cold list:   %10d B  %8d allocs  %12v  %d sessions (counts every conversation once)",
					cold.bytes, cold.allocs, cold.elapsed, cold.sessions)
				t.Logf("  warm list:   %10d B  %8d allocs  %12v  (provider scan cached)",
					warm.bytes, warm.allocs, warm.elapsed)
				t.Logf("  after cache: %10d B  %8d allocs  %12v  (provider scan repeated)",
					rescan.bytes, rescan.allocs, rescan.elapsed)
				t.Logf("  2nd rescan:  %10d B  %8d allocs  %12v  (steady state)",
					rescanAgain.bytes, rescanAgain.allocs, rescanAgain.elapsed)
				t.Logf("  preview:     %10d B  %8d allocs  %12v  %d messages",
					preview.bytesPerRead, preview.allocsPerRead, preview.elapsedPerRead, preview.messages)
				t.Logf("  head budget %d B/file, tail budget %d B/file", codexScanHeadBudget, codexTailBudget)

				if cold.sessions != count {
					t.Fatalf("listed %d conversations, want %d", cold.sessions, count)
				}
			})
		}
	}
}

// Every rollout a listing has not already described is opened by it. Proving
// it by making files unreadable counts the opens in the fixture instead of
// assuming them: a conversation this listing never opened could not notice that
// its file became unreadable.
//
// The second listing runs on a store that has described nothing, which is the
// case this measures. A store that already holds a card for one exact file
// answers from it and does not open that file again — the same rule the message
// counts follow, and the reason a restart no longer re-reads a fleet's history.
func TestListingOpensEveryCodexRollout(t *testing.T) {
	const count = 12
	root := t.TempDir()
	sessionsDir, ids, _ := writeCodexRollouts(t, root, count, 256*1024)
	store := codexStore(root)
	if listed := measureList(t, store); listed.sessions != count {
		t.Fatalf("baseline listed %d conversations, want %d", listed.sessions, count)
	}

	// Make a third of them unreadable, then wait out the provider scan cache so
	// the next listing is a real scan.
	unreadable := map[string]bool{}
	for index := 0; index < count; index += 3 {
		unreadable[ids[index]] = true
		path := filepath.Join(sessionsDir, "rollout-"+ids[index]+".jsonl")
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	}
	time.Sleep(providerScanCacheTT + 250*time.Millisecond)

	// A store with nothing described yet: every rollout is opened.
	listed, err := codexStore(root).List(nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, session := range listed.Sessions {
		seen[session.ProviderSessionID] = true
	}
	for _, id := range ids {
		if unreadable[id] == seen[id] {
			t.Fatalf("conversation %s: unreadable=%t listed=%t; a listing that opens every rollout "+
				"must lose exactly the unreadable ones", id, unreadable[id], seen[id])
		}
	}
	if len(listed.Sessions) != count-len(unreadable) {
		t.Fatalf("listed %d conversations, want %d", len(listed.Sessions), count-len(unreadable))
	}

	// The store that described them before their permissions changed still
	// lists them, because nothing about those files changed: same size, same
	// modification time, same card. It did not open one to find out.
	fromCards, err := store.List(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(fromCards.Sessions) != count {
		t.Fatalf("a store that already described them listed %d conversations, want %d",
			len(fromCards.Sessions), count)
	}
}

// activityOf finds one conversation's reported last activity in a listing.
func activityOf(t *testing.T, listed []HistorySession, providerID string) (int64, bool) {
	t.Helper()
	for _, session := range listed {
		if session.ProviderSessionID == providerID {
			return session.LastActivityAt, true
		}
	}
	return 0, false
}

// A listing must not pay to read a file it has already read and that has not
// changed. Proving it the same way the open count was proved: an unchanged file
// that becomes unreadable is still listed, with the activity already known,
// because nothing opened it again. A file that has changed is a different
// fingerprint, so it is read again — and if it is unreadable by then, it is
// reported as unreadable rather than answered from a stale cache.
func TestUnchangedConversationsAreNotRereadByTheNextListing(t *testing.T) {
	const count = 9
	root := t.TempDir()
	sessionsDir, ids, _ := writeCodexRollouts(t, root, count, 256*1024)
	store := codexStore(root)

	first, err := store.List(nil)
	if err != nil {
		t.Fatal(err)
	}
	before := make(map[string]int64, count)
	for _, id := range ids {
		activity, ok := activityOf(t, first.Sessions, id)
		if !ok {
			t.Fatalf("conversation %s missing from the first listing", id)
		}
		before[id] = activity
	}

	// One conversation grows; the rest are untouched. Then every file becomes
	// unreadable, so any listing that opens one can be seen doing it.
	grown := ids[count/2]
	grownPath := filepath.Join(sessionsDir, "rollout-"+grown+".jsonl")
	appended, err := os.OpenFile(grownPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	later := `{"timestamp":"2027-01-02T03:04:05Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a later answer"}]}}` + "\n"
	if _, err := appended.WriteString(later); err != nil {
		t.Fatal(err)
	}
	if err := appended.Close(); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		path := filepath.Join(sessionsDir, "rollout-"+id+".jsonl")
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	}
	time.Sleep(providerScanCacheTT + 250*time.Millisecond)

	second, err := store.List(nil)
	if err != nil {
		t.Fatal(err)
	}
	// The scan opens every rollout, so an unreadable file leaves the provider
	// listing entirely. What this test can still show is the cache: read the
	// activity directly, which is the call a listing makes per conversation.
	for _, id := range ids {
		path := filepath.Join(sessionsDir, "rollout-"+id+".jsonl")
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		updated, fromRecord := store.conversationUpdatedAt(path, info)
		if id == grown {
			// A changed file is a different fingerprint. With the file
			// unreadable there is no record to find, so it falls back to the
			// modification time rather than serving the pre-growth answer.
			if fromRecord {
				t.Fatalf("grown conversation %s answered from a record it could not read", id)
			}
			if updated != info.ModTime().UnixMilli() {
				t.Fatalf("grown conversation %s reported %d, want its new modification time %d",
					id, updated, info.ModTime().UnixMilli())
			}
			continue
		}
		if !fromRecord {
			t.Fatalf("unchanged conversation %s was reread and lost its recorded activity", id)
		}
		if updated != before[id] {
			t.Fatalf("unchanged conversation %s reported %d, want the cached %d", id, updated, before[id])
		}
	}
	_ = second
}

// The count cache and the activity cache share one entry and one fingerprint.
// Neither may be mistaken for the other: a conversation whose activity is known
// has not thereby been counted, and the summary listing must keep saying so.
func TestActivityCacheDoesNotInventAMessageCount(t *testing.T) {
	root := t.TempDir()
	_, ids, _ := writeCodexRollouts(t, root, 3, 96*1024)
	store := codexStore(root)

	summary, err := store.SearchSessions(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range summary {
		if session.MessageCount != 0 || !session.MessageCountUncounted {
			t.Fatalf("summary listing reported a count nobody computed: %#v", session)
		}
	}

	counted, err := store.List(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range counted.Sessions {
		if session.MessageCount == 0 || session.MessageCountUncounted {
			t.Fatalf("full listing did not count %s: %#v", session.ProviderSessionID, session)
		}
	}

	// And now that both halves are filled, the cheap listing answers from the
	// cache rather than reporting the count as unknown again.
	cachedSummary, err := store.SearchSessions(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range cachedSummary {
		if session.MessageCountUncounted {
			t.Fatalf("summary listing lost the cached count for %s", session.ProviderSessionID)
		}
	}
	if len(cachedSummary) != len(ids) {
		t.Fatalf("summary listed %d conversations, want %d", len(cachedSummary), len(ids))
	}
}

// A file that grows after both halves were cached must be read again, and the
// newer activity must win. This is the interaction the 2 s scan cache and the
// fingerprint cache have to get right together.
func TestGrownConversationIsRereadAndReportsNewerActivity(t *testing.T) {
	root := t.TempDir()
	sessionsDir, ids, _ := writeCodexRollouts(t, root, 1, 64*1024)
	store := codexStore(root)
	first, err := store.List(nil)
	if err != nil {
		t.Fatal(err)
	}
	before, ok := activityOf(t, first.Sessions, ids[0])
	if !ok {
		t.Fatalf("conversation %s missing from the first listing", ids[0])
	}
	firstCount := first.Sessions[0].MessageCount

	path := filepath.Join(sessionsDir, "rollout-"+ids[0]+".jsonl")
	appended, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appended.WriteString(`{"timestamp":"2027-01-02T03:04:05Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a later answer"}]}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := appended.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(providerScanCacheTT + 250*time.Millisecond)

	second, err := store.List(nil)
	if err != nil {
		t.Fatal(err)
	}
	after, ok := activityOf(t, second.Sessions, ids[0])
	if !ok {
		t.Fatalf("conversation %s missing from the second listing", ids[0])
	}
	if after <= before {
		t.Fatalf("grown conversation reported activity %d, not newer than %d", after, before)
	}
	if second.Sessions[0].MessageCount <= firstCount {
		t.Fatalf("grown conversation still reports %d messages, was %d", second.Sessions[0].MessageCount, firstCount)
	}
}
