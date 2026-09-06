package watch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexInputCacheReusesUnchangedFilesAndInvalidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	writes := func(text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writes("old")
	reads := 0
	cache := codexInputCache{read: func(path, expected string) (bool, error) {
		reads++
		data, err := os.ReadFile(path)
		return strings.Contains(string(data), expected), err
	}}
	for i := 0; i < 10; i++ {
		if cache.matches(path, "new") {
			t.Fatal("unexpected match")
		}
	}
	if reads != 1 {
		t.Fatalf("unchanged file read %d times", reads)
	}
	writes("old new")
	if !cache.matches(path, "new") || reads != 2 {
		t.Fatal("append not observed")
	}
	if !cache.matches(path, "old") || reads != 3 {
		t.Fatal("changed input not checked")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := path + ".replacement"
	if err := os.WriteFile(replacement, []byte("not new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if cache.matches(path, "old") || reads != 4 {
		t.Fatal("same-size, same-mtime replacement reused stale match")
	}
}

func TestCodexInputCacheDoesNotCacheFailuresOrChangingFiles(t *testing.T) {
	for _, change := range []bool{false, true} {
		t.Run(fmt.Sprint(change), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rollout.jsonl")
			if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			reads := 0
			cache := codexInputCache{read: func(path, expected string) (bool, error) {
				reads++
				if reads == 1 {
					if change {
						return false, os.WriteFile(path, []byte("new input"), 0o600)
					}
					return false, errors.New("temporary read failure")
				}
				return true, nil
			}}
			cache.matches(path, "input")
			if !cache.matches(path, "input") || reads != 2 {
				t.Fatal("cached a failed or raced read")
			}
		})
	}
}

func TestCachedCodexResolutionStillDetectsNewAmbiguousCandidate(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	one := filepath.Join(codexDateDir(root, now), "rollout-one.jsonl")
	writeRolloutFixtureWithPrompt(t, one, "/tmp/cwd", now, "same")
	cache := codexInputCache{}
	options := CodexResolveOptions{CWD: "/tmp/cwd", CreatedAt: now.Add(-time.Second), SessionsDir: root,
		Now: now, ExpectedInput: "same", inputMatcher: cache.matches}
	if got := ResolveCodexRolloutPath(options); got.Path != one {
		t.Fatalf("first match: %+v", got)
	}
	writeRolloutFixtureWithPrompt(t, filepath.Join(codexDateDir(root, now), "rollout-two.jsonl"), "/tmp/cwd", now, "same")
	if got := ResolveCodexRolloutPath(options); got.Path != "" || got.Reason != CodexInputAmbiguous {
		t.Fatalf("cache hid ambiguity: %+v", got)
	}
}

func TestCodexInputCacheIsBoundedAndRetriesEvictedFiles(t *testing.T) {
	cache := codexInputCache{}
	root := t.TempDir()
	first := filepath.Join(root, "0")
	for i := 0; i <= codexInputCacheLimit; i++ {
		path := filepath.Join(root, fmt.Sprint(i))
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		cache.matches(path, "input")
	}
	if len(cache.entries) != codexInputCacheLimit || len(cache.order) != codexInputCacheLimit {
		t.Fatal("unbounded cache")
	}
	if _, exists := cache.entries[first]; exists {
		t.Fatal("oldest file not evicted")
	}
	cache.matches(first, "input")
	if _, exists := cache.entries[first]; !exists {
		t.Fatal("evicted file not reconsidered")
	}
}

func BenchmarkCachedCodexInputMatchLargeToolOutputs(b *testing.B) {
	path := filepath.Join(b.TempDir(), "rollout.jsonl")
	line := `{"type":"response_item","payload":{"type":"function_call_output","output":"` + strings.Repeat("x", 256*1024) + `"}}` + "\n"
	if err := os.WriteFile(path, []byte(strings.Repeat(line, 16)), 0o600); err != nil {
		b.Fatal(err)
	}
	cache := codexInputCache{}
	cache.matches(path, "not present")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cache.matches(path, "not present")
	}
}
