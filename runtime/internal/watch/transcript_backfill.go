package watch

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// BackfillResult describes one attempt to copy an existing provider transcript
// into a mirror.
type BackfillResult struct {
	// Copied counts records this call added. A second run over an unchanged
	// transcript copies nothing, because record identity is rebuilt from the
	// mirror when it is opened.
	Copied int
	// AlreadyPresent counts records the mirror already held. On a repeat run
	// this is the whole conversation.
	AlreadyPresent int
	// Skipped counts records that could not be stored -- a line past the scan
	// cap, or an append refused because the mirror is at its size cap. The
	// count is returned rather than logged so a lossy backfill can never be
	// mistaken for a complete one.
	Skipped int
	// Capped reports that the mirror stopped accepting appends.
	Capped bool
}

// Complete reports whether every record in the provider transcript is now in
// the mirror.
func (r BackfillResult) Complete() bool { return r.Skipped == 0 && !r.Capped }

// BackfillTranscriptMirror copies an existing provider transcript into a
// mirror.
//
// A live session needs none of this: the watcher re-reads its provider file
// from offset zero on every attach, so anything it is watching is mirrored
// already. The gap is conversations nobody is watching any more -- ended
// sessions whose provider transcript still exists but whose runner is long
// gone. Those are the ones the provider's retention timer deletes next, and
// after that they are unrecoverable.
//
// The provider file is opened read-only and never modified: it belongs to the
// provider, and this is a copy, not a move. Appends are idempotent, so running
// this repeatedly is safe and is the intended way to use it.
func BackfillTranscriptMirror(providerPath string, options TranscriptMirrorOptions) (BackfillResult, error) {
	var result BackfillResult
	if providerPath == "" || options.Path == "" {
		return result, errors.New("backfill needs both a provider transcript and a mirror path")
	}
	source, err := os.Open(providerPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The provider already pruned it. Nothing to rescue, and that is
			// not an error the caller can act on.
			return result, nil
		}
		return result, fmt.Errorf("read provider transcript %s: %w", providerPath, err)
	}
	defer source.Close()

	mirror, err := OpenTranscriptMirror(options)
	if err != nil {
		return result, err
	}
	defer func() {
		_ = mirror.Close()
	}()
	mirror.NoteProviderPath(providerPath)
	// One call reads the provider file from its first record, so it is one
	// replay pass. Without this a second backfill would renumber repeated
	// records and duplicate every one of them into the mirror.
	mirror.BeginPass()

	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 0, 64*1024), transcriptScanLineCap)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		stored, appendErr := mirror.Append(line)
		if appendErr != nil {
			return result, fmt.Errorf("write mirror %s: %w", options.Path, appendErr)
		}
		switch {
		case stored:
			result.Copied++
		case mirror.Meta().Capped:
			result.Skipped++
		default:
			result.AlreadyPresent++
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		// A record longer than the scan cap is the usual cause. Everything
		// before it is already stored, so report the loss and keep what was
		// rescued rather than discarding the whole conversation.
		result.Skipped++
		result.Capped = mirror.Meta().Capped
		if syncErr := mirror.Sync(); syncErr != nil {
			return result, syncErr
		}
		return result, fmt.Errorf("provider transcript %s could not be read to the end: %w", providerPath, scanErr)
	}
	result.Capped = mirror.Meta().Capped
	if syncErr := mirror.Sync(); syncErr != nil {
		return result, syncErr
	}
	return result, nil
}
