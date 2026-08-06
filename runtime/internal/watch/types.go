// Package watch resolves and tails Claude Code session JSONL files and Codex
// rollout files. All filesystem inputs can be overridden so tests and callers
// can operate entirely inside isolated state roots.
package watch

import (
	"context"
	"sync"
	"sync/atomic"
)

// SessionEvent is the canonical structured event shape served by sessionsd.
// Claude events pass through unchanged; Codex records are normalized into the
// same map shape before they are emitted.
type SessionEvent map[string]any

// FileWatcher is the common watcher result for Claude and Codex session files.
// Events and errors are ordered as observed by the single serialized tail
// loop. Working is used by Codex lifecycle records; Claude never writes it.
type FileWatcher struct {
	Events  <-chan SessionEvent
	Working <-chan bool
	Errors  <-chan error

	events  chan SessionEvent
	working chan bool
	errors  chan error

	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
	input     chan string
	pathMu    sync.RWMutex
	path      string
	skipped   atomic.Int64
}

func newFileWatcher() (*FileWatcher, context.Context) {
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan SessionEvent, 4096)
	working := make(chan bool, 256)
	errors := make(chan error, 32)
	w := &FileWatcher{
		Events:  events,
		Working: working,
		Errors:  errors,
		events:  events,
		working: working,
		errors:  errors,
		cancel:  cancel,
		done:    make(chan struct{}),
		input:   make(chan string, 16),
	}
	return w, ctx
}

// ExpectInput gives a provider watcher the exact logical user message which
// was just submitted to its terminal. Fresh Codex processes can create several
// same-CWD rollout files at nearly the same time; matching the provider-owned
// user record is the only safe way to bind without guessing by timestamp.
// Watchers which do not need the hint simply ignore it.
func (w *FileWatcher) ExpectInput(text string) {
	if w == nil || text == "" {
		return
	}
	select {
	case w.input <- text:
	case <-w.done:
	}
}

// Path returns the file currently being followed, or an empty string while
// resolution is pending or deliberately ambiguous.
func (w *FileWatcher) Path() string {
	w.pathMu.RLock()
	defer w.pathMu.RUnlock()
	return w.path
}

// SkippedRecords reports how many provider records this watcher could not
// forward because a single record exceeded the in-memory record bound. The
// torn-record policy in internal/integrations/errors.go requires that a skip is
// counted and reachable by the caller: a degraded tail must never be
// indistinguishable from a clean one. The provider file is left untouched, so
// the skipped bytes are still on disk.
//
// The count is of skip events. A provider file that is replaced or truncated is
// re-read from the start, so a record that stays oversized is counted again on
// each pass, which is the honest reading of "records this watcher did not
// deliver".
func (w *FileWatcher) SkippedRecords() int {
	if w == nil {
		return 0
	}
	return int(w.skipped.Load())
}

func (w *FileWatcher) noteSkippedRecords(count int) {
	if count > 0 {
		w.skipped.Add(int64(count))
	}
}

func (w *FileWatcher) setPath(path string) {
	w.pathMu.Lock()
	w.path = path
	w.pathMu.Unlock()
}

// Close stops all polling and fsnotify watches. It is safe to call more than
// once and waits until the tail goroutine has released its resources.
func (w *FileWatcher) Close() {
	w.closeOnce.Do(func() {
		w.cancel()
		<-w.done
	})
}

func (w *FileWatcher) finish() {
	close(w.events)
	close(w.working)
	close(w.errors)
	close(w.done)
}

func (w *FileWatcher) emitEvent(ctx context.Context, event SessionEvent) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case w.events <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

func (w *FileWatcher) emitWorking(ctx context.Context, working bool) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case w.working <- working:
		return true
	case <-ctx.Done():
		return false
	}
}

func (w *FileWatcher) emitError(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case w.errors <- err:
		return true
	case <-ctx.Done():
		return false
	}
}
