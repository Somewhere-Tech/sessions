package integrations

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"
)

// A profile of the first history listing after a restart, against this
// machine's own provider directories.
//
// It is opt-in (SESSIONS_PROFILE_HISTORY_DIRS=1) and never runs in CI: it reads
// directories that belong to the person running it. It reads and never writes
// them — the only file it creates is a copy of the retained cache inside the
// test's temporary directory, so the daemon's own cache is untouched — and it
// prints counts and timings, never a path, a title, or a line of anybody's
// conversation.
//
//	SESSIONS_PROFILE_HISTORY_DIRS=1 go test ./internal/integrations/ \
//	  -run FirstListingAgainstRealDirectories -v -cpuprofile /tmp/history.prof
func TestFirstListingAgainstRealDirectories(t *testing.T) {
	if os.Getenv("SESSIONS_PROFILE_HISTORY_DIRS") != "1" {
		t.Skip("set SESSIONS_PROFILE_HISTORY_DIRS=1 to profile against this machine's own history")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	claudeProjects := envOr("SESSIONS_PROFILE_CLAUDE_PROJECTS", filepath.Join(home, ".claude", "projects"))
	codexSessions := envOr("SESSIONS_PROFILE_CODEX_SESSIONS", filepath.Join(home, ".codex", "sessions"))
	claudeHistory := envOr("SESSIONS_PROFILE_CLAUDE_HISTORY", filepath.Join(home, ".claude", "history.jsonl"))
	runnerState := envOr("SESSIONS_PROFILE_RUNNER_STATE", filepath.Join(home, ".local", "state", "sessions", "runners"))
	realCache := envOr("SESSIONS_PROFILE_CACHE", filepath.Join(home, ".local", "state", "sessions", "history-cache.json"))

	work := t.TempDir()
	cachePath := filepath.Join(work, "history-cache.json")
	copied := copyIfPresent(t, realCache, cachePath)
	t.Logf("retained cache copied=%v entries=%d by provider: %s",
		copied, cacheEntryCount(t, cachePath), entriesByProvider(t, cachePath))

	for _, directory := range []struct {
		label string
		path  string
	}{{"claude projects", claudeProjects}, {"codex sessions", codexSessions}} {
		files, bytes := countTree(directory.path)
		t.Logf("%-16s %5d files %10.1f MiB", directory.label, files, float64(bytes)/(1024*1024))
	}
	if info, statErr := os.Stat(claudeHistory); statErr == nil {
		t.Logf("%-16s %5d files %10.1f MiB", "claude history", 1, float64(info.Size())/(1024*1024))
	}

	options := HistoryOptions{
		RunnerStateDir: runnerState, ClaudeProjectsDir: claudeProjects,
		CodexSessionsDir: codexSessions, ClaudeHistoryPath: claudeHistory,
		Machine: "profile", DiscoverProviderHistory: true, CachePath: cachePath,
	}

	// With no retained cache at all: every conversation counted from scratch,
	// which is what a machine pays the first time and what a stale entry costs
	// again for the files it names.
	empty := filepath.Join(work, "empty-cache.json")
	fromScratch := timeList(t, NewHistoryStore(withCache(options, empty)))
	time.Sleep(providerScanCacheTT + 250*time.Millisecond)

	// Cold: a fresh store, exactly as a restarted daemon starts, with whatever
	// the retained cache already knew.
	cold := timeList(t, NewHistoryStore(options))
	// And again, on another fresh store, after the first listing has written
	// everything it learned. If this is still slow, the retained cache is not
	// covering what the listing pays for.
	time.Sleep(providerScanCacheTT + 250*time.Millisecond)
	secondCold := timeList(t, NewHistoryStore(options))
	warm := timeList(t, nil, cold.store)
	// Outside the provider-scan TTL, which is what a person's second visit a
	// few seconds later actually pays.
	time.Sleep(providerScanCacheTT + 250*time.Millisecond)
	rescan := timeList(t, nil, cold.store)

	t.Logf("scratch%7.2fs  %6d rows  %5d uncounted  %8.1f MiB allocated (no retained cache)",
		fromScratch.elapsed.Seconds(), fromScratch.rows, fromScratch.uncounted, mib(fromScratch.bytes))
	t.Logf("cold  %8.2fs  %6d rows  %5d uncounted  %8.1f MiB allocated",
		cold.elapsed.Seconds(), cold.rows, cold.uncounted, mib(cold.bytes))
	t.Logf("cold2 %8.2fs  %6d rows  %5d uncounted  %8.1f MiB allocated (fresh store, cache just written)",
		secondCold.elapsed.Seconds(), secondCold.rows, secondCold.uncounted, mib(secondCold.bytes))
	t.Logf("warm  %8.2fs  %6d rows  %5d uncounted  %8.1f MiB allocated",
		warm.elapsed.Seconds(), warm.rows, warm.uncounted, mib(warm.bytes))
	t.Logf("rescan%8.2fs  %6d rows  %5d uncounted  %8.1f MiB allocated (provider scan repeated)",
		rescan.elapsed.Seconds(), rescan.rows, rescan.uncounted, mib(rescan.bytes))
	cold.store.cacheMu.Lock()
	inMemory := len(cold.store.cache)
	counted, activity := 0, 0
	for _, entry := range cold.store.cache {
		if entry.counted {
			counted++
		}
		if entry.activity {
			activity++
		}
	}
	dirty := cold.store.cacheDirty
	cold.store.cacheMu.Unlock()
	t.Logf("in-memory cache after: %d entries (%d counted, %d activity) dirty=%v",
		inMemory, counted, activity, dirty)
	t.Logf("retained cache after: %d entries by provider: %s",
		cacheEntryCount(t, cachePath), entriesByProvider(t, cachePath))

	// The claim this harness exists to check.
	if cold.elapsed > 3*warm.elapsed {
		t.Logf("COLD IS %.1fx WARM — the first listing after a restart is still paying for something",
			cold.elapsed.Seconds()/warm.elapsed.Seconds())
	}
}

type listRun struct {
	store     *HistoryStore
	elapsed   time.Duration
	rows      int
	uncounted int
	bytes     uint64
}

func timeList(t *testing.T, store *HistoryStore, reuse ...*HistoryStore) listRun {
	t.Helper()
	if len(reuse) > 0 {
		store = reuse[0]
	}
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
	uncounted := 0
	for _, session := range listed.Sessions {
		if session.MessageCountUncounted {
			uncounted++
		}
	}
	return listRun{
		store: store, elapsed: elapsed, rows: len(listed.Sessions),
		uncounted: uncounted, bytes: after.TotalAlloc - before.TotalAlloc,
	}
}

func withCache(options HistoryOptions, path string) HistoryOptions {
	options.CachePath = path
	return options
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func copyIfPresent(t *testing.T, from, to string) bool {
	t.Helper()
	raw, err := os.ReadFile(from)
	if err != nil {
		return false
	}
	if err := os.WriteFile(to, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return true
}

func cacheEntryCount(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var stored persistedHistoryCache
	if json.Unmarshal(raw, &stored) != nil {
		return -1
	}
	return len(stored.Entries)
}

// countTree counts files and bytes without opening anything.
func countTree(root string) (files int, bytes int64) {
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil //nolint:nilerr // an unreadable corner is not this count's business
		}
		files++
		bytes += info.Size()
		return nil
	})
	return files, bytes
}

func mib(bytes uint64) float64 { return float64(bytes) / (1024 * 1024) }

// entriesByProvider is printed by the harness so the retained cache's coverage
// is visible as a number rather than assumed.
func entriesByProvider(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return "(none)"
	}
	var stored persistedHistoryCache
	if json.Unmarshal(raw, &stored) != nil {
		return "(unreadable)"
	}
	counts := map[string]int{}
	for _, entry := range stored.Entries {
		switch {
		case filepath.Ext(entry.Path) != ".jsonl":
			counts["other"]++
		case containsSegment(entry.Path, ".codex"):
			counts["codex"]++
		case containsSegment(entry.Path, ".claude"):
			counts["claude"]++
		default:
			counts["managed"]++
		}
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := ""
	for _, key := range keys {
		out += fmt.Sprintf("%s=%d ", key, counts[key])
	}
	return out
}

func containsSegment(path, segment string) bool {
	dir := path
	for {
		next := filepath.Dir(dir)
		if filepath.Base(dir) == segment {
			return true
		}
		if next == dir {
			return false
		}
		dir = next
	}
}
