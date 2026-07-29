package watch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	codexBackfillLineLimit = 2_000
	codexReadByteLimit     = 16 * 1024 * 1024
)

// CodexWatcherOptions configures rollout resolution and tailing. RolloutPath
// and SessionsDir support deterministic synthetic fixtures; production leaves
// them empty.
type CodexWatcherOptions struct {
	CWD          string
	Args         []string
	CreatedAt    time.Time
	SessionsDir  string
	RolloutPath  string
	InitialDelay time.Duration
	PollInterval time.Duration
	Now          func() time.Time
}

type codexTail struct {
	watcher *FileWatcher
	ctx     context.Context
	hints   *notifyHints
	options CodexWatcherOptions

	path      string
	fileInfo  os.FileInfo
	offset    int64
	buffer    string
	lineIndex int
}

// WatchCodexRollout starts a backfilling Codex rollout watcher.
func WatchCodexRollout(options CodexWatcherOptions) *FileWatcher {
	if options.InitialDelay <= 0 {
		options.InitialDelay = 800 * time.Millisecond
	}
	if options.PollInterval <= 0 {
		options.PollInterval = 2 * time.Second
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	watcher, ctx := newFileWatcher()
	tail := &codexTail{
		watcher: watcher,
		ctx:     ctx,
		hints:   newNotifyHints(),
		options: options,
	}
	go tail.run()
	return watcher
}

func (tail *codexTail) run() {
	defer tail.watcher.finish()
	defer tail.hints.close()

	initial := time.NewTimer(tail.options.InitialDelay)
	defer initial.Stop()
	poll := time.NewTicker(tail.options.PollInterval)
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
			// The independent poll loop remains authoritative for liveness.
		}
	}
}

func (tail *codexTail) tick() {
	if tail.ctx.Err() != nil {
		return
	}
	now := tail.options.Now()
	for _, dir := range CodexWatchDirs(tail.options.SessionsDir, now, tail.options.CreatedAt) {
		tail.hints.add(dir)
	}
	if tail.path != "" {
		tail.hints.add(filepath.Dir(tail.path))
	}

	if tail.options.RolloutPath != "" {
		tail.attach(tail.options.RolloutPath)
	} else {
		resolution := ResolveCodexRolloutPath(CodexResolveOptions{
			CWD:         tail.options.CWD,
			Args:        tail.options.Args,
			CreatedAt:   tail.options.CreatedAt,
			SessionsDir: tail.options.SessionsDir,
			Now:         now,
		})
		if resolution.Path != "" && (tail.path == "" ||
			(tail.path != resolution.Path && resolution.Reason == CodexResumeMatch)) {
			tail.attach(resolution.Path)
		}
	}
	tail.read()
}

func (tail *codexTail) attach(path string) {
	if tail.path == path {
		return
	}
	if tail.path != "" {
		tail.hints.remove(tail.path)
	}
	tail.path = path
	tail.resetReadState()
	tail.fileInfo = nil
	tail.watcher.setPath(path)
	tail.hints.add(filepath.Dir(path))
	tail.hints.add(path)
}

func (tail *codexTail) detach() {
	tail.hints.remove(tail.path)
	tail.path = ""
	tail.fileInfo = nil
	tail.resetReadState()
	tail.watcher.setPath("")
}

func (tail *codexTail) resetReadState() {
	tail.offset = 0
	tail.buffer = ""
	tail.lineIndex = 0
}

func (tail *codexTail) read() {
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
	needsBackfill := tail.fileInfo == nil || !os.SameFile(tail.fileInfo, info) || info.Size() < tail.offset
	if needsBackfill {
		tail.backfill(file, info)
		// Re-stat once for an append that raced the attach snapshot. The byte
		// offset is already the exact snapshot boundary, so this is a no-gap,
		// no-duplicate live handoff.
		info, err = file.Stat()
		if err != nil {
			return
		}
	}

	end := info.Size()
	for tail.offset < end && tail.ctx.Err() == nil {
		liveEnd := minInt64(end, tail.offset+codexReadByteLimit)
		chunk, readErr := readRange(file, tail.offset, liveEnd-tail.offset)
		if readErr != nil || len(chunk) == 0 {
			return
		}
		tail.offset += int64(len(chunk))
		tail.consume(chunk)
	}
	tail.hints.add(tail.path)
}

func (tail *codexTail) backfill(file *os.File, info os.FileInfo) {
	snapshotEnd := info.Size()
	windowStart := snapshotEnd - codexReadByteLimit
	if windowStart < 0 {
		windowStart = 0
	}
	window, err := readRange(file, windowStart, snapshotEnd-windowStart)
	if err != nil || tail.ctx.Err() != nil {
		return
	}
	tail.resetReadState()
	tail.fileInfo = info
	tail.offset = windowStart + int64(len(window))
	replayStart := boundedBackfillStart(window, windowStart)
	tail.consume(window[replayStart:])
}

func boundedBackfillStart(buffer []byte, windowStart int64) int {
	usableStart := 0
	if windowStart > 0 {
		firstNewline := -1
		for index, value := range buffer {
			if value == '\n' {
				firstNewline = index
				break
			}
		}
		if firstNewline < 0 {
			return len(buffer)
		}
		usableStart = firstNewline + 1
	}

	lines := 0
	if len(buffer) > usableStart && buffer[len(buffer)-1] != '\n' {
		lines = 1
	}
	for index := len(buffer) - 1; index >= usableStart; index-- {
		if buffer[index] != '\n' {
			continue
		}
		lines++
		if lines > codexBackfillLineLimit {
			return index + 1
		}
	}
	return usableStart
}

func (tail *codexTail) consume(chunk []byte) {
	tail.buffer += string(chunk)
	for {
		newline := strings.IndexByte(tail.buffer, '\n')
		if newline < 0 {
			return
		}
		line := tail.buffer[:newline]
		tail.buffer = tail.buffer[newline+1:]
		lineIndex := tail.lineIndex
		tail.lineIndex++
		if strings.TrimSpace(line) == "" {
			continue
		}
		var decoded map[string]any
		if json.Unmarshal([]byte(line), &decoded) != nil {
			continue
		}
		normalized := NormalizeCodexRolloutLine(decoded, CodexNormalizeContext{
			RolloutBasename: filepath.Base(tail.path),
			LineIndex:       lineIndex,
		})
		for _, event := range normalized.Events {
			if !tail.watcher.emitEvent(tail.ctx, event) {
				return
			}
		}
		if normalized.Working != nil && !tail.watcher.emitWorking(tail.ctx, *normalized.Working) {
			return
		}
	}
}
