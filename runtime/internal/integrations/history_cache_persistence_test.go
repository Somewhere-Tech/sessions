package integrations

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// codexStoreWithCache is codexStore with the fingerprints kept between runs, so
// a second store is a restarted daemon rather than a new machine.
func codexStoreWithCache(root, cachePath string) *HistoryStore {
	return NewHistoryStore(HistoryOptions{
		RunnerStateDir:    filepath.Join(root, "runners"),
		ClaudeProjectsDir: filepath.Join(root, "claude-projects"),
		CodexSessionsDir:  filepath.Join(root, "codex-sessions"),
		Machine:           "fixture-mac", DiscoverProviderHistory: true,
		CachePath: cachePath,
	})
}

// The first listing after a restart pays for every conversation again, because
// the message counts and recorded activity it computed died with the process.
// Kept on disk, a restarted daemon's first listing costs what its second one
// would.
func TestFirstListingAfterARestartUsesTheRetainedCounts(t *testing.T) {
	const count = 40
	root := t.TempDir()
	cachePath := filepath.Join(root, "state", "history-cache.json")
	writeCodexRollouts(t, root, count, 8*1024*1024)

	first := codexStoreWithCache(root, cachePath)
	cold := measureList(t, first)
	warm := measureList(t, first)
	if cold.sessions != count || warm.sessions != count {
		t.Fatalf("listed %d then %d conversations, want %d", cold.sessions, warm.sessions, count)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("a listing that counted %d conversations kept nothing: %v", count, err)
	}

	// A fresh store, as after a restart: same files, same cache file, no memory.
	restarted := measureList(t, codexStoreWithCache(root, cachePath))
	if restarted.sessions != count {
		t.Fatalf("restarted listing returned %d conversations, want %d", restarted.sessions, count)
	}
	// Without the retained cache this is the cold cost again. The claim is only
	// that it is far closer to warm than to cold; the numbers are logged.
	t.Logf("cold %d B / %v · warm %d B / %v · after restart %d B / %v",
		cold.bytes, cold.elapsed, warm.bytes, warm.elapsed, restarted.bytes, restarted.elapsed)
	if restarted.bytes >= cold.bytes/2 {
		t.Fatalf("first listing after a restart allocated %d B against a cold %d B and a warm %d B",
			restarted.bytes, cold.bytes, warm.bytes)
	}
}

// The retained answer is about one exact file. A conversation that changed
// while the daemon was down is counted again, and its new activity is reported.
func TestAChangedConversationIsRecountedAfterARestart(t *testing.T) {
	const count = 6
	root := t.TempDir()
	cachePath := filepath.Join(root, "state", "history-cache.json")
	sessionsDir, ids, _ := writeCodexRollouts(t, root, count, 512*1024)

	first := codexStoreWithCache(root, cachePath)
	listed, err := first.List(nil)
	if err != nil {
		t.Fatal(err)
	}
	grown := ids[count/2]
	before, ok := messageCountOf(listed.Sessions, grown)
	if !ok {
		t.Fatalf("conversation %s missing from the first listing", grown)
	}

	// It gains a message while nothing is running.
	path := filepath.Join(sessionsDir, "rollout-"+grown+".jsonl")
	appended, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
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
	time.Sleep(providerScanCacheTT + 250*time.Millisecond)

	restarted, err := codexStoreWithCache(root, cachePath).List(nil)
	if err != nil {
		t.Fatal(err)
	}
	after, ok := messageCountOf(restarted.Sessions, grown)
	if !ok {
		t.Fatalf("conversation %s missing after the restart", grown)
	}
	if after != before+1 {
		t.Fatalf("changed conversation reports %d messages, want %d: a retained count outlived its file",
			after, before+1)
	}
	// Every other conversation still reports what it did.
	for _, id := range ids {
		if id == grown {
			continue
		}
		was, _ := messageCountOf(listed.Sessions, id)
		is, present := messageCountOf(restarted.Sessions, id)
		if !present || was != is {
			t.Fatalf("conversation %s reported %d then %d", id, was, is)
		}
	}
}

// A cache file that is not a cache costs one line in the log and nothing else.
func TestACorruptCacheFileIsIgnored(t *testing.T) {
	const count = 4
	root := t.TempDir()
	cachePath := filepath.Join(root, "state", "history-cache.json")
	writeCodexRollouts(t, root, count, 128*1024)
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte("{not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	listed, err := codexStoreWithCache(root, cachePath).List(nil)
	if err != nil {
		t.Fatalf("a corrupt cache file broke the listing: %v", err)
	}
	if len(listed.Sessions) != count {
		t.Fatalf("listed %d conversations, want %d", len(listed.Sessions), count)
	}
}

// An entry for a file that has since vanished is ignored, and the file it names
// is not resurrected into the listing.
func TestARetainedEntryForAMissingFileIsIgnored(t *testing.T) {
	const count = 3
	root := t.TempDir()
	cachePath := filepath.Join(root, "state", "history-cache.json")
	sessionsDir, ids, _ := writeCodexRollouts(t, root, count, 96*1024)
	if _, err := codexStoreWithCache(root, cachePath).List(nil); err != nil {
		t.Fatal(err)
	}
	removed := ids[0]
	if err := os.Remove(filepath.Join(sessionsDir, "rollout-"+removed+".jsonl")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(providerScanCacheTT + 250*time.Millisecond)

	listed, err := codexStoreWithCache(root, cachePath).List(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := messageCountOf(listed.Sessions, removed); present {
		t.Fatalf("conversation %s is still listed after its file was deleted", removed)
	}
	if len(listed.Sessions) != count-1 {
		t.Fatalf("listed %d conversations, want %d", len(listed.Sessions), count-1)
	}
}

// What is written is bounded and readable, and holds what the readers need.
func TestTheRetainedCacheIsBoundedAndDescribesItsFiles(t *testing.T) {
	const count = 5
	root := t.TempDir()
	cachePath := filepath.Join(root, "state", "history-cache.json")
	writeCodexRollouts(t, root, count, 160*1024)
	if _, err := codexStoreWithCache(root, cachePath).List(nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	var stored persistedHistoryCache
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Version != historyCacheFileVersion {
		t.Fatalf("cache version %d, want %d", stored.Version, historyCacheFileVersion)
	}
	if len(stored.Entries) == 0 || len(stored.Entries) > maxHistoryCacheEntries {
		t.Fatalf("cache holds %d entries, want between 1 and %d", len(stored.Entries), maxHistoryCacheEntries)
	}
	for _, entry := range stored.Entries {
		if entry.Path == "" || entry.Size == 0 || entry.ModTimeNano == 0 {
			t.Fatalf("entry %#v does not describe a file", entry)
		}
		if !strings.HasSuffix(entry.Path, ".jsonl") {
			t.Fatalf("entry path %q is not a transcript", entry.Path)
		}
		info, statErr := os.Stat(entry.Path)
		if statErr != nil {
			t.Fatalf("entry names a file that is not there: %v", statErr)
		}
		if info.Size() != entry.Size || info.ModTime().UnixNano() != entry.ModTimeNano {
			t.Fatalf("entry fingerprint does not match the file it names")
		}
	}
}

func messageCountOf(sessions []HistorySession, providerID string) (int, bool) {
	for _, session := range sessions {
		if session.ProviderSessionID == providerID {
			return session.MessageCount, true
		}
	}
	return 0, false
}

// The scan that builds every resumable card reads the head of every
// conversation — half a megabyte per Codex rollout. Kept across a restart, a
// conversation nobody has touched is not opened at all, and the card it
// produced is the same card.
func TestRetainedCardsSurviveARestartAndAreNotRereadable(t *testing.T) {
	const count = 12
	root := t.TempDir()
	cachePath := filepath.Join(root, "state", "history-cache.json")
	sessionsDir, ids, _ := writeCodexRollouts(t, root, count, 4*1024*1024)

	first, err := codexStoreWithCache(root, cachePath).List(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Sessions) != count {
		t.Fatalf("first listing returned %d conversations, want %d", len(first.Sessions), count)
	}

	// Every rollout becomes unreadable. A listing that opens one can be seen
	// doing it; a listing answering from what it already described cannot.
	for _, id := range ids {
		path := filepath.Join(sessionsDir, "rollout-"+id+".jsonl")
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	}
	time.Sleep(providerScanCacheTT + 250*time.Millisecond)

	restarted, err := codexStoreWithCache(root, cachePath).List(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(restarted.Sessions) != count {
		t.Fatalf("after a restart the listing returned %d conversations, want %d — it re-read files it had already described",
			len(restarted.Sessions), count)
	}
	// The same cards, not merely the same number of them.
	for _, before := range first.Sessions {
		after, present := sessionByID(restarted.Sessions, before.ID)
		if !present {
			t.Fatalf("conversation %s disappeared after the restart", before.ID)
		}
		if after.Name != before.Name || after.ProviderSessionID != before.ProviderSessionID || after.CWD != before.CWD {
			t.Fatalf("conversation %s came back different:\n before %#v\n after  %#v", before.ID, before, after)
		}
	}
}

// A conversation that grew is described again, because its card comes from a
// file that is no longer the file the card was read from.
func TestAGrownConversationIsDescribedAgainAfterARestart(t *testing.T) {
	const count = 4
	root := t.TempDir()
	cachePath := filepath.Join(root, "state", "history-cache.json")
	sessionsDir, ids, _ := writeCodexRollouts(t, root, count, 256*1024)
	if _, err := codexStoreWithCache(root, cachePath).List(nil); err != nil {
		t.Fatal(err)
	}

	grown := ids[count/2]
	path := filepath.Join(sessionsDir, "rollout-"+grown+".jsonl")
	appended, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
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
	// It also becomes unreadable, so a listing that describes it again fails to
	// read it and drops the row — which is exactly how we can tell it tried.
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	time.Sleep(providerScanCacheTT + 250*time.Millisecond)

	restarted, err := codexStoreWithCache(root, cachePath).List(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := messageCountOf(restarted.Sessions, grown); present {
		t.Fatal("a conversation that changed was answered from the card read before it changed")
	}
	if len(restarted.Sessions) != count-1 {
		t.Fatalf("listed %d conversations, want %d", len(restarted.Sessions), count-1)
	}
}

func sessionByID(sessions []HistorySession, id string) (HistorySession, bool) {
	for _, session := range sessions {
		if session.ID == id {
			return session, true
		}
	}
	return HistorySession{}, false
}
