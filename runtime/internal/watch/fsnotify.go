package watch

import (
	"os"

	"github.com/fsnotify/fsnotify"
)

// notifyHints deliberately treats fsnotify as a latency optimization only.
// Every watcher also polls, and every hint causes a fresh stat/read rather than
// being trusted as a complete description of what changed.
type notifyHints struct {
	watcher *fsnotify.Watcher
	watched map[string]struct{}
}

func newNotifyHints() *notifyHints {
	native, err := fsnotify.NewWatcher()
	if err != nil {
		return &notifyHints{}
	}
	return &notifyHints{watcher: native, watched: make(map[string]struct{})}
}

func (h *notifyHints) add(path string) {
	if h.watcher == nil || path == "" {
		return
	}
	if _, ok := h.watched[path]; ok {
		return
	}
	if _, err := os.Stat(path); err != nil {
		return
	}
	if err := h.watcher.Add(path); err == nil {
		h.watched[path] = struct{}{}
	}
}

func (h *notifyHints) forgetRemoved(event fsnotify.Event) {
	if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		delete(h.watched, event.Name)
	}
}

func (h *notifyHints) remove(path string) {
	if h.watcher == nil || path == "" {
		return
	}
	_ = h.watcher.Remove(path)
	delete(h.watched, path)
}

func (h *notifyHints) events() <-chan fsnotify.Event {
	if h.watcher == nil {
		return nil
	}
	return h.watcher.Events
}

func (h *notifyHints) errors() <-chan error {
	if h.watcher == nil {
		return nil
	}
	return h.watcher.Errors
}

func (h *notifyHints) close() {
	if h.watcher != nil {
		_ = h.watcher.Close()
	}
}
