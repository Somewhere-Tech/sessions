// Package watch resolves and tails Claude Code session JSONL files and Codex
// rollout files. All filesystem inputs can be overridden so tests and callers
// can operate entirely inside isolated state roots.
package watch

import (
	"context"
	"sync"
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
	pathMu    sync.RWMutex
	path      string
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
	}
	return w, ctx
}

// Path returns the file currently being followed, or an empty string while
// resolution is pending or deliberately ambiguous.
func (w *FileWatcher) Path() string {
	w.pathMu.RLock()
	defer w.pathMu.RUnlock()
	return w.path
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
