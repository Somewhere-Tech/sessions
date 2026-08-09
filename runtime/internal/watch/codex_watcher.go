package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// WHY THIS WATCHER DOES NOT TEE INTO A TRANSCRIPT MIRROR
//
// The Claude watcher keeps Sessions' own append-only copy of every provider
// record it sees, because Claude Code deletes project transcripts on a ~30-day
// retention timer -- 81 of 93 recorded conversations on the development machine
// are already gone. The exposure looks identical here: the conversation lives in
// a provider-owned rollout file and Sessions keeps no copy. It is not, and the
// decision to leave this watcher unmirrored is deliberate.
//
// Measured on ~/.codex/sessions on the development machine (176 rollouts):
//
//  1. No retention. Rollouts survive unbroken back to 2026-04-09, roughly four
//     months, with none missing. Codex prunes nothing. This is the single
//     largest difference: mirroring Claude rescues files that are actively being
//     destroyed, and there is nothing here being destroyed.
//
//  2. Compaction does not lose the file. 42 "compacted" records appear across
//     the corpus, so Codex does compact context -- but it appends a marker and
//     keeps writing. The pre-compaction records stay on disk. Compaction shrinks
//     what the model sees, not what the rollout contains.
//
//  3. The rollout is append-only across resumes. One rollout carried six
//     session_meta records and was still being appended seventeen days after it
//     was created, reaching 1.15 GB. Codex resumes into the original file rather
//     than starting a new one, so a conversation is never scattered and never
//     orphaned by a renamed working directory -- rollouts are addressed by date
//     and UUID under sessions/YYYY/MM/DD/, not by an encoded cwd. Neither the
//     bucket collapse nor the rename-orphaning that mirroring rescues for Claude
//     can happen here.
//
// And teeing specifically would be the wrong mechanism even if mirroring were
// wanted, because this tail is a BOUNDED-WINDOW reader. It backfills at most
// codexReadByteLimit bytes and replays at most codexBackfillLineLimit lines,
// where the Claude tail re-reads its file from offset zero on every attach.
// Teeing here would silently produce a mirror holding the last 2000 lines of a
// 79,610-line conversation while presenting itself as a complete transcript.
// That is worse than no mirror: the failure mode a mirror exists to prevent is
// exactly "Sessions thought it had the conversation".
//
// Codex mirroring is therefore available, but through BackfillTranscriptMirror,
// which reads a rollout from its first byte. That function is already provider
// neutral -- it copies JSONL verbatim and keys records by content, which is what
// Codex rollouts need since they carry no uuid field. See
// TestBackfillMirrorsCodexRolloutLosslessly. What is missing is only the caller:
// `sessions transcripts` has a Claude arm and no Codex arm.
//
// The mirror cap remains finite, and BackfillResult reports Skipped and Capped
// rather than claiming success if a future rollout exceeds it. The default is
// deliberately above the largest rollout measured in the real corpus.
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
	// RequireInputMatch prevents a fresh same-CWD process from guessing which
	// provider rollout belongs to it. Resume IDs and explicit RolloutPath values
	// remain authoritative without a submitted-message hint.
	RequireInputMatch bool
}

type codexTail struct {
	watcher *FileWatcher
	ctx     context.Context
	hints   *notifyHints
	options CodexWatcherOptions

	path          string
	fileInfo      os.FileInfo
	offset        int64
	lines         lineBuffer
	anchor        readAnchor
	expectedInput string

	reportedSkips int
	skipReported  bool
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
		case input := <-tail.watcher.input:
			tail.expectedInput = normalizedCodexInput(input)
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
			CWD:           tail.options.CWD,
			Args:          tail.options.Args,
			CreatedAt:     tail.options.CreatedAt,
			SessionsDir:   tail.options.SessionsDir,
			Now:           now,
			ExpectedInput: tail.expectedInput,
		})
		if tail.options.RequireInputMatch && ExtractCodexResumeID(tail.options.Args) == "" && tail.expectedInput == "" {
			resolution.Path = ""
		}
		if resolution.Path != "" && (tail.path == "" || tail.path != resolution.Path &&
			(resolution.Reason == CodexResumeMatch || resolution.Reason == CodexInputMatch)) {
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
	tail.skipReported = false
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
	tail.lines.reset()
	tail.anchor.reset()
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
	// The rollout is append-only in normal operation -- Codex even appends across
	// resumes rather than starting a new file -- so the stat-derived signals are
	// usually enough. They are not sufficient: an in-place rewrite at or above
	// the current offset looks identical to a plain append through stat alone,
	// and resuming into it replays the middle of a record that no longer exists.
	// Re-reading the bytes at the resume point is the only check that separates
	// the two, and a failed check costs one bounded re-backfill.
	needsBackfill := tail.fileInfo == nil ||
		!os.SameFile(tail.fileInfo, info) ||
		info.Size() < tail.offset ||
		!tail.anchor.intact(file, tail.offset)
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
		tail.anchor.advance(chunk)
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
	// Anchor on the window's trailing bytes, not on the replayed slice: the
	// offset sits at the end of the window, so that is the span a later resume
	// has to find unchanged.
	tail.anchor.advance(window)
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
	// Same bounded carry as the Claude tail: a rollout record longer than the
	// bound is skipped and counted rather than held whole. The record position is
	// taken from the buffer's own record count so that a skipped record still
	// advances it -- the synthesized event ids are "<basename>:<index>", and an
	// index that silently shifted would rename every event after the skip.
	defer tail.reportSkippedRecords()
	tail.lines.feed(chunk)
	for {
		line, ok := tail.lines.next()
		if !ok {
			return
		}
		lineIndex := tail.lines.records - 1
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var decoded map[string]any
		if json.Unmarshal(line, &decoded) != nil {
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

// reportSkippedRecords mirrors the Claude tail: count every skip, surface it
// once per attached rollout, and leave the provider file untouched.
func (tail *codexTail) reportSkippedRecords() {
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
		"skipped a record longer than %d bytes in %s; it stays in the rollout file and is counted, later records still stream",
		tail.lines.limit(), tail.path,
	))
}
