package integrations

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// What a conversation's transcript costs to describe is paid once per file, and
// then never again — until the daemon restarts. On a machine holding 561
// records and six hundred provider conversations, the first listing after every
// install takes about seventeen seconds against one second afterwards, and the
// difference is entirely this: message counts and recorded activity, computed
// per file and lost with the process.
//
// So the fingerprints outlive the process. What is written down is not an
// answer about history — it is what was computed about one exact file, and
// every entry is checked against that file's current size and modification time
// before it is used, exactly as the in-memory cache already checks. A file that
// changed, moved or vanished simply misses and is recomputed. A file that
// cannot be parsed leaves an empty cache and one line in the log.

const (
	historyCacheFileVersion = 1

	// historyCacheSaveInterval is the floor between writes. A listing that
	// learned nothing writes nothing; a client asking repeatedly while files
	// change cannot turn the cache into a write loop.
	historyCacheSaveInterval = 5 * time.Second
)

type persistedHistoryEntry struct {
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	ModTimeNano int64  `json:"mod_time_nano"`
	Count       int    `json:"count,omitempty"`
	Skipped     int    `json:"skipped,omitempty"`
	Counted     bool   `json:"counted,omitempty"`
	RecordedMS  int64  `json:"recorded_ms,omitempty"`
	HasRecord   bool   `json:"has_record,omitempty"`
	Activity    bool   `json:"activity,omitempty"`
}

type persistedHistoryCache struct {
	Version int                     `json:"version"`
	Entries []persistedHistoryEntry `json:"entries"`
}

// loadPersistedCache fills the in-memory cache from the last run. Nothing here
// is trusted about the files themselves: the entries carry the fingerprint each
// answer was computed at, and the readers compare it before answering.
func (h *HistoryStore) loadPersistedCache() {
	if h.options.CachePath == "" {
		return
	}
	raw, err := os.ReadFile(h.options.CachePath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[history] cannot read the retained message-count cache, starting empty: %v", err)
		}
		return
	}
	var stored persistedHistoryCache
	if err := json.Unmarshal(raw, &stored); err != nil {
		log.Printf("[history] cannot use the retained message-count cache, starting empty: %v", err)
		return
	}
	if stored.Version != historyCacheFileVersion {
		log.Printf("[history] retained message-count cache is version %d, not %d; starting empty",
			stored.Version, historyCacheFileVersion)
		return
	}
	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()
	for _, entry := range stored.Entries {
		if entry.Path == "" || len(h.cache) >= maxHistoryCacheEntries {
			continue
		}
		h.cacheClock++
		h.cache[entry.Path] = historyCacheEntry{
			size: entry.Size, modTimeNano: entry.ModTimeNano,
			count: entry.Count, skipped: entry.Skipped, counted: entry.Counted,
			recordedMS: entry.RecordedMS, hasRecord: entry.HasRecord, activity: entry.Activity,
			used: h.cacheClock,
		}
	}
}

// persistIfDirty writes the cache when a listing learned something new. It runs
// after the listing has answered, so the person waiting for their history never
// waits for this file.
func (h *HistoryStore) persistIfDirty() {
	if h.options.CachePath == "" {
		return
	}
	h.cacheMu.Lock()
	now := time.Now()
	if !h.cacheDirty || (!h.cacheSavedAt.IsZero() && now.Sub(h.cacheSavedAt) < historyCacheSaveInterval) {
		h.cacheMu.Unlock()
		return
	}
	h.cacheDirty = false
	h.cacheSavedAt = now
	stored := persistedHistoryCache{Version: historyCacheFileVersion, Entries: make([]persistedHistoryEntry, 0, len(h.cache))}
	for path, entry := range h.cache {
		stored.Entries = append(stored.Entries, persistedHistoryEntry{
			Path: path, Size: entry.size, ModTimeNano: entry.modTimeNano,
			Count: entry.count, Skipped: entry.skipped, Counted: entry.counted,
			RecordedMS: entry.recordedMS, HasRecord: entry.hasRecord, Activity: entry.activity,
		})
	}
	h.cacheMu.Unlock()

	if err := writeHistoryCacheFile(h.options.CachePath, stored); err != nil {
		historyCacheWriteFailure.Do(func() {
			log.Printf("[history] cannot retain the message-count cache, every restart will recount: %v", err)
		})
	}
}

var historyCacheWriteFailure sync.Once

// writeHistoryCacheFile replaces the file in one step. A half-written cache is
// a cache that reads as corrupt on the next boot, and this exists to save a
// restart, not to cost one.
func writeHistoryCacheFile(path string, stored persistedHistoryCache) error {
	encoded, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}
