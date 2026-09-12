package integrations

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Reported from the MacBook, 11 September: across four restarts on the same day
// with the same persisted cache, the first listing's store stage cost 0.3 s,
// 4.0 s, and two figures in between. A store stage is slow for two very
// different reasons — it had to open conversation files again, or it was
// competing with the daemon's own startup work — and the listing could not say
// which. Now it counts: cards served from the fingerprints, and cards read back
// out of a file.
func TestAListingCountsTheCardsItServedAndTheOnesItHadToRead(t *testing.T) {
	const count = 12
	root := t.TempDir()
	cachePath := filepath.Join(root, "state", "history-cache.json")
	_, _, _ = writeCodexRollouts(t, root, count, 512*1024)

	store := codexStoreWithCache(root, cachePath)
	if _, err := store.List(nil); err != nil {
		t.Fatal(err)
	}
	cold := store.CardCounts()
	if cold.Read != count || cold.Hit != 0 {
		t.Fatalf("the first listing read %d cards and served %d from cache, want %d read", cold.Read, cold.Hit, count)
	}

	// The next scan of the same unchanged files opens nothing.
	store.providerCachedAt = time.Time{}
	if _, err := store.List(nil); err != nil {
		t.Fatal(err)
	}
	warm := store.CardCounts()
	if warm.Hit-cold.Hit != count || warm.Read != cold.Read {
		t.Fatalf("the second listing served %d cards and read %d more, want %d served and none read",
			warm.Hit-cold.Hit, warm.Read-cold.Read, count)
	}

	// One conversation grows. Exactly one card is read again, and the rest are
	// still answered from what was already known about those exact files.
	grown := filepath.Join(root, "codex-sessions", "2026", "09", "10",
		"rollout-00000003-aaaa-4aaa-8aaa-aaaaaaaaaaaa.jsonl")
	appendLine(t, grown)
	store.providerCachedAt = time.Time{}
	if _, err := store.List(nil); err != nil {
		t.Fatal(err)
	}
	changed := store.CardCounts()
	if changed.Read-warm.Read != 1 {
		t.Fatalf("one changed conversation caused %d re-reads", changed.Read-warm.Read)
	}
	if changed.Hit-warm.Hit != count-1 {
		t.Fatalf("%d cards were served after one file changed, want %d", changed.Hit-warm.Hit, count-1)
	}
}

// A restarted daemon reads the persisted file, so its first listing serves the
// cards it kept rather than opening every conversation again. This is what
// distinguishes a slow cold store from a store that was merely competing for
// the machine: the counts differ even when the seconds do not.
func TestARestartedStoreServesItsCardsFromThePersistedFile(t *testing.T) {
	const count = 10
	root := t.TempDir()
	cachePath := filepath.Join(root, "state", "history-cache.json")
	_, _, _ = writeCodexRollouts(t, root, count, 256*1024)

	first := codexStoreWithCache(root, cachePath)
	if _, err := first.List(nil); err != nil {
		t.Fatal(err)
	}
	if read := first.CardCounts().Read; read != count {
		t.Fatalf("the first daemon read %d cards, want %d", read, count)
	}

	restarted := codexStoreWithCache(root, cachePath)
	if _, err := restarted.List(nil); err != nil {
		t.Fatal(err)
	}
	after := restarted.CardCounts()
	t.Logf("a restarted store served %d cards from the persisted file and read %d", after.Hit, after.Read)
	if after.Hit != count || after.Read != 0 {
		t.Fatalf("a restarted store served %d cards and read %d, want all %d served", after.Hit, after.Read, count)
	}
}

func appendLine(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(
		`{"timestamp":"2026-09-11T00:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"one more"}]}}` + "\n",
	); err != nil {
		t.Fatal(err)
	}
}
