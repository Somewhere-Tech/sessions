package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	claudeEmittedCap    = 60_000
	claudeEmittedTrimTo = 40_000
	claudeReadChunk     = 16 * 1024 * 1024
)

// ClaudeWatcherOptions configures a Claude Code JSONL watcher. ProjectDir is
// an explicit project-directory override for synthetic fixtures. ProjectsDir
// overrides the config root while preserving resolved and legacy CWD probes.
type ClaudeWatcherOptions struct {
	CWD             string
	ClaudeSessionID string
	ProjectDir      string
	ProjectsDir     string
	InitialDelay    time.Duration
	PollInterval    time.Duration
}

type claudeTail struct {
	watcher *FileWatcher
	ctx     context.Context
	hints   *notifyHints

	projectDirs []string
	sessionID   string
	path        string
	fileInfo    os.FileInfo
	offset      int64
	buffer      string

	emitted      map[string]struct{}
	emittedOrder []string
	unresolved   bool
}

// WatchClaudeSession starts resolving and tailing a Claude session file.
func WatchClaudeSession(options ClaudeWatcherOptions) (*FileWatcher, error) {
	projectDirs := []string{options.ProjectDir}
	if options.ProjectDir == "" && options.ProjectsDir != "" {
		projectDirs = ClaudeProjectDirsUnder(options.ProjectsDir, options.CWD)
	} else if options.ProjectDir == "" {
		var err error
		projectDirs, err = ClaudeProjectDirs(options.CWD)
		if err != nil {
			return nil, err
		}
	}
	if options.InitialDelay <= 0 {
		options.InitialDelay = 800 * time.Millisecond
	}
	if options.PollInterval <= 0 {
		options.PollInterval = 2 * time.Second
	}

	watcher, ctx := newFileWatcher()
	tail := &claudeTail{
		watcher:     watcher,
		ctx:         ctx,
		hints:       newNotifyHints(),
		projectDirs: projectDirs,
		sessionID:   options.ClaudeSessionID,
		emitted:     make(map[string]struct{}),
	}
	go tail.run(options.InitialDelay, options.PollInterval)
	return watcher, nil
}

// WatchSessionFile is the Go equivalent of the normative TypeScript name.
func WatchSessionFile(options ClaudeWatcherOptions) (*FileWatcher, error) {
	return WatchClaudeSession(options)
}

func (tail *claudeTail) run(initialDelay, pollInterval time.Duration) {
	defer tail.watcher.finish()
	defer tail.hints.close()

	initial := time.NewTimer(initialDelay)
	defer initial.Stop()
	poll := time.NewTicker(pollInterval)
	defer poll.Stop()

	for {
		select {
		case <-tail.ctx.Done():
			return
		case <-initial.C:
			tail.tick()
		case <-poll.C:
			tail.tick()
		case event, ok := <-tail.hints.events():
			if !ok {
				continue
			}
			tail.hints.forgetRemoved(event)
			tail.tick()
		case _, ok := <-tail.hints.errors():
			if !ok {
				continue
			}
			// Polling is the source of liveness; fsnotify errors are hints too.
		}
	}
}

func (tail *claudeTail) tick() {
	if tail.ctx.Err() != nil {
		return
	}
	for _, dir := range tail.projectDirs {
		tail.hints.add(dir)
	}
	resolution := tail.resolve()
	if resolution.Path != "" {
		if tail.path == "" || (tail.path != resolution.Path && resolution.Reason == ClaudeExact) {
			tail.attach(resolution.Path)
		}
	} else if tail.path == "" && !tail.unresolved {
		tail.unresolved = true
		tail.watcher.emitError(tail.ctx, fmt.Errorf(
			"unresolved JSONL for %s in %s: %s",
			valueOr(tail.sessionID, "(no id)"), strings.Join(tail.projectDirs, ", "), resolution.Reason,
		))
	}
	tail.read()
}

func (tail *claudeTail) resolve() ClaudeResolution {
	return resolveClaudeJSONLDirs(tail.projectDirs, tail.sessionID)
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func (tail *claudeTail) attach(path string) {
	if tail.path == path {
		return
	}
	if tail.path != "" {
		tail.hints.remove(tail.path)
	}
	tail.path = path
	tail.fileInfo = nil
	tail.offset = 0
	tail.buffer = ""
	tail.watcher.setPath(path)
	tail.hints.add(filepath.Dir(path))
	tail.hints.add(path)
}

func (tail *claudeTail) detach() {
	tail.hints.remove(tail.path)
	tail.path = ""
	tail.fileInfo = nil
	tail.offset = 0
	tail.buffer = ""
	tail.watcher.setPath("")
}

func (tail *claudeTail) read() {
	if tail.path == "" || tail.ctx.Err() != nil {
		return
	}
	file, err := os.Open(tail.path)
	if err != nil {
		tail.detach()
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		tail.detach()
		return
	}
	if (tail.fileInfo != nil && !os.SameFile(tail.fileInfo, info)) || info.Size() < tail.offset {
		tail.offset = 0
		tail.buffer = ""
	}
	tail.fileInfo = info

	end := info.Size()
	for tail.offset < end && tail.ctx.Err() == nil {
		length := minInt64(end-tail.offset, claudeReadChunk)
		chunk, readErr := readRange(file, tail.offset, length)
		if readErr != nil || len(chunk) == 0 {
			return
		}
		tail.offset += int64(len(chunk))
		tail.consume(chunk)
	}
	tail.hints.add(tail.path)
}

func (tail *claudeTail) consume(chunk []byte) {
	// Concatenating raw byte strings preserves a UTF-8 codepoint split across
	// reads until its newline-delimited JSON record is complete.
	tail.buffer += string(chunk)
	for {
		newline := strings.IndexByte(tail.buffer, '\n')
		if newline < 0 {
			return
		}
		line := tail.buffer[:newline]
		tail.buffer = tail.buffer[newline+1:]
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event SessionEvent
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		if uuid, ok := event["uuid"].(string); ok {
			if _, duplicate := tail.emitted[uuid]; duplicate {
				continue
			}
			tail.emitted[uuid] = struct{}{}
			tail.emittedOrder = append(tail.emittedOrder, uuid)
			tail.trimEmitted()
		}
		if !tail.watcher.emitEvent(tail.ctx, event) {
			return
		}
	}
}

func (tail *claudeTail) trimEmitted() {
	if len(tail.emitted) <= claudeEmittedCap {
		return
	}
	drop := len(tail.emittedOrder) - claudeEmittedTrimTo
	for _, uuid := range tail.emittedOrder[:drop] {
		delete(tail.emitted, uuid)
	}
	tail.emittedOrder = append([]string(nil), tail.emittedOrder[drop:]...)
}

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}
