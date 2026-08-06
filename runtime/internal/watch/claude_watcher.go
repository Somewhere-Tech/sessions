package watch

import (
	"bytes"
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

	// MirrorPath enables Sessions' own durable copy of the provider transcript.
	// The watcher is the right place to tee from: it is already the one
	// component that resolves, tails, and parses the provider file, and it
	// re-reads that file from offset zero on every attach, so a mirror opened
	// here backfills a conversation that started before Sessions was watching.
	// An empty path disables mirroring and leaves behaviour exactly as before.
	MirrorPath     string
	SessionID      string
	MirrorCapBytes int64

	// MaxRecordBytes bounds one JSONL record held in memory while the tail waits
	// for its newline. Zero selects maxTranscriptRecordBytes, which is what every
	// production caller wants; fixtures set it small so a pathological record can
	// be exercised without writing a 64 MiB file.
	MaxRecordBytes int
}

type claudeTail struct {
	watcher *FileWatcher
	ctx     context.Context
	hints   *notifyHints

	projectDirs []string
	cwd         string
	sessionID   string
	path        string
	fileInfo    os.FileInfo
	offset      int64
	lines       lineBuffer
	anchor      readAnchor

	emitted       map[string]struct{}
	emittedOrder  []string
	unresolved    bool
	reportedSkips int
	skipReported  bool

	mirror      *TranscriptMirror
	mirrorWrote bool
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
		cwd:         options.CWD,
		sessionID:   options.ClaudeSessionID,
		emitted:     make(map[string]struct{}),
	}
	tail.lines.max = options.MaxRecordBytes
	if options.MirrorPath != "" {
		mirror, err := OpenTranscriptMirror(TranscriptMirrorOptions{
			Path:              options.MirrorPath,
			SessionID:         options.SessionID,
			ProviderSessionID: options.ClaudeSessionID,
			Tool:              "claude",
			CapBytes:          options.MirrorCapBytes,
		})
		if err != nil {
			// A mirror that cannot be opened must not cost the user their live
			// session view, which is what the watcher is primarily for. The
			// failure is surfaced once and the watcher continues unmirrored.
			watcher.emitError(ctx, fmt.Errorf("open transcript mirror %s: %w", options.MirrorPath, err))
		} else {
			tail.mirror = mirror
		}
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
	defer tail.mirror.Close()

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
	// The cwd is passed so a bucket shared by two working directories can be
	// split by what each candidate transcript recorded for itself, rather than
	// leaving the live session with no structured events at all.
	return resolveClaudeJSONLDirsForCWD(tail.projectDirs, tail.sessionID, tail.cwd)
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
	tail.skipReported = false
	tail.restartAtZero()
	tail.mirror.NoteProviderPath(path)
	tail.watcher.setPath(path)
	tail.hints.add(filepath.Dir(path))
	tail.hints.add(path)
}

func (tail *claudeTail) detach() {
	// Losing the provider file is the moment the mirror has to be trustworthy,
	// so everything observed so far is forced to disk before the path is
	// dropped rather than at some later shutdown that may never happen.
	_ = tail.mirror.Sync()
	tail.hints.remove(tail.path)
	tail.path = ""
	tail.fileInfo = nil
	tail.restartAtZero()
	tail.watcher.setPath("")
}

// restartAtZero puts the tail back at the start of a provider file. The mirror
// must be told before any record is offered to it, because a replay from zero
// re-presents content the mirror has already stored and only a fresh pass
// numbers those repeats the same way it numbered them the first time.
func (tail *claudeTail) restartAtZero() {
	tail.offset = 0
	tail.lines.reset()
	tail.anchor.reset()
	tail.mirror.BeginPass()
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
	replaced := tail.fileInfo != nil && !os.SameFile(tail.fileInfo, info)
	truncated := info.Size() < tail.offset
	// The identity and size checks above catch a replaced or shrunken file. They
	// cannot catch an in-place rewrite that leaves the file the same size or
	// larger, which is the case that resumes mid-stream and reads records that
	// no longer exist. Only the bytes settle that, so ask them -- but only when
	// nothing cheaper has already decided, because the anchor costs a read.
	rewritten := !replaced && !truncated && !tail.anchor.intact(file, tail.offset)
	if replaced || truncated || rewritten {
		// The provider replaced, truncated, or rewrote its file. Re-reading from
		// zero plus the mirror's own deduplication means Sessions keeps the union
		// of what the conversation ever contained, not just what survived the
		// provider's rewrite.
		tail.mirror.NoteRotation()
		tail.restartAtZero()
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
		tail.anchor.advance(chunk)
		tail.consume(chunk)
	}
	// Flush once per read batch rather than once per record: a turn arrives as
	// a burst, and the provider file is still the backstop for anything a crash
	// loses between batches.
	if tail.mirrorWrote {
		tail.mirrorWrote = false
		_ = tail.mirror.Sync()
	}
	tail.hints.add(tail.path)
}

func (tail *claudeTail) consume(chunk []byte) {
	// Carrying raw bytes preserves a UTF-8 codepoint split across reads until its
	// newline-delimited JSON record is complete. The carry is bounded: a record
	// longer than the bound is skipped and counted rather than being allowed to
	// hold the whole file in memory.
	defer tail.reportSkippedRecords()
	tail.lines.feed(chunk)
	for {
		line, ok := tail.lines.next()
		if !ok {
			return
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event SessionEvent
		if json.Unmarshal(line, &event) != nil {
			continue
		}
		// Tee before the tail's own deduplication. That set is capped and
		// evicts old UUIDs, so a re-read of a long conversation can present
		// records the tail no longer recognizes. The mirror keeps an uncapped
		// identity set of its own and is the component that must not miss one.
		// The raw line is stored, not the re-encoded event, so the mirror stays
		// byte-identical provider JSONL.
		if appended, err := tail.mirror.Append(line); err == nil {
			tail.mirrorWrote = tail.mirrorWrote || appended
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

// reportSkippedRecords publishes any newly skipped oversized records. The count
// is what the torn-record policy requires a caller to be able to read back; the
// error stream carries the first skip per attached file as well, because a
// number nobody polls is not a surfaced number, and one error per skip could
// flood a bounded channel on a file full of them.
func (tail *claudeTail) reportSkippedRecords() {
	if tail.lines.skipped == tail.reportedSkips {
		return
	}
	tail.watcher.noteSkippedRecords(tail.lines.skipped - tail.reportedSkips)
	tail.reportedSkips = tail.lines.skipped
	if tail.skipReported {
		return
	}
	tail.skipReported = true
	tail.watcher.emitError(tail.ctx, fmt.Errorf(
		"skipped a record longer than %d bytes in %s; it stays in the provider file and is counted, later records still stream",
		tail.lines.limit(), tail.path,
	))
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
