package watch

import "os"

const codexInputCacheLimit = 256

type codexInputCacheEntry struct {
	info    os.FileInfo
	matched bool
}

// Owned by one watcher's serial loop. Keep only the latest submitted input and
// bounded file fingerprints, never rollout contents. A growing/replaced file,
// changed input, or failed read must still run the original matching check.
type codexInputCache struct {
	expected string
	entries  map[string]codexInputCacheEntry
	order    []string
	next     int
	read     func(string, string) (bool, error)
}

func sameCodexInputFile(before, after os.FileInfo) bool {
	return os.SameFile(before, after) && before.Size() == after.Size() &&
		before.ModTime().Equal(after.ModTime()) && before.Mode() == after.Mode()
}

func (cache *codexInputCache) matches(path, expected string) bool {
	expected = normalizedCodexInput(expected)
	if expected == "" {
		return false
	}
	if cache.entries == nil || cache.expected != expected {
		cache.expected = expected
		cache.entries = make(map[string]codexInputCacheEntry)
		cache.order, cache.next = nil, 0
	}
	before, err := os.Stat(path)
	if err != nil {
		delete(cache.entries, path)
		return false
	}
	if entry, ok := cache.entries[path]; ok && sameCodexInputFile(entry.info, before) {
		return entry.matched
	}
	read := cache.read
	if read == nil {
		read = readCodexUserInput
	}
	matched, readErr := read(path, expected)
	after, statErr := os.Stat(path)
	if readErr == nil && statErr == nil && sameCodexInputFile(before, after) {
		cache.remember(path, codexInputCacheEntry{info: after, matched: matched})
	} else {
		delete(cache.entries, path)
	}
	return matched
}

func (cache *codexInputCache) remember(path string, entry codexInputCacheEntry) {
	if _, exists := cache.entries[path]; !exists {
		if len(cache.order) < codexInputCacheLimit {
			cache.order = append(cache.order, path)
		} else {
			delete(cache.entries, cache.order[cache.next])
			cache.order[cache.next] = path
			cache.next = (cache.next + 1) % codexInputCacheLimit
		}
	}
	cache.entries[path] = entry
}
