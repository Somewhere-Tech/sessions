package watch

import "bytes"

// maxTranscriptRecordBytes bounds one newline-delimited provider record while a
// tail waits for its terminating newline.
//
// Without a bound a tail that accumulates `buffer += string(chunk)` grows to the
// size of the file whenever the file has no newline in it: quadratic copying,
// and full-file residency for a transcript that is 1.15 GB on this development
// machine.
//
// The number is measured rather than guessed. Across every Claude transcript in
// ~/.claude/projects and every Codex rollout in ~/.codex/sessions on this
// machine the longest single record is 16,917,872 bytes -- one record inside
// that 1.15 GB rollout -- and the longest Claude record is 634,358 bytes. A
// 16 MiB bound would therefore already discard real conversation, which is the
// thing this package exists to prevent, so the bound is transcriptScanLineCap:
// four times the largest record ever observed here.
//
// Sharing the mirror's scan cap is the load-bearing part, not a convenience. A
// record the tail forwards is also written to the mirror, and a record the
// mirror's own reader cannot scan back is a record its identity set forgets on
// the next open -- so the tail must never accept a record longer than the mirror
// can read.
const maxTranscriptRecordBytes = transcriptScanLineCap

// lineBuffer splits chunk reads into whole newline-delimited records with a hard
// cap on what it holds in memory.
//
// A record longer than the cap is skipped and counted, never silently dropped
// and never allowed to grow the process: see the torn-record policy in
// internal/integrations/errors.go, which this implements for the live tail. The
// bytes stay in the provider file untouched, so a later forensic read still has
// them.
type lineBuffer struct {
	// carry holds the incomplete record left over from earlier chunks. It is
	// released rather than reused so a returned line can never alias bytes a
	// later append overwrites.
	carry []byte
	// pending is the not-yet-scanned remainder of the current chunk.
	pending []byte
	// max is the largest record this buffer will assemble, in bytes, excluding
	// the newline. Zero means maxTranscriptRecordBytes.
	max int

	// dropping means the record currently being scanned already exceeded max and
	// has been counted; every further byte of it is discarded until its newline.
	dropping bool

	// records counts complete records observed since the last reset, including
	// blank ones and skipped ones, so a caller numbering records by position
	// stays correct across a skip.
	records int

	// skipped counts records dropped for exceeding max. It is cumulative for the
	// life of the buffer and deliberately survives reset, because callers report
	// it as a running total rather than a per-pass one.
	skipped int
}

func (b *lineBuffer) limit() int {
	if b.max <= 0 {
		return maxTranscriptRecordBytes
	}
	return b.max
}

// reset returns the buffer to the start of a fresh replay. The skip total is
// kept; everything describing position within the file is discarded.
func (b *lineBuffer) reset() {
	b.carry = nil
	b.pending = nil
	b.dropping = false
	b.records = 0
}

// feed stages one chunk. The caller must have drained the previous chunk with
// next until it reported no further record; a tail only stops draining early
// when its context is already cancelled and it will not read again.
func (b *lineBuffer) feed(chunk []byte) {
	b.pending = chunk
}

// next returns the next complete record, or ok=false when the staged chunk is
// exhausted. The returned slice is valid until the next call to feed or next.
func (b *lineBuffer) next() ([]byte, bool) {
	limit := b.limit()
	for len(b.pending) > 0 {
		newline := bytes.IndexByte(b.pending, '\n')
		if newline < 0 {
			segment := b.pending
			b.pending = nil
			if b.dropping {
				return nil, false
			}
			if len(b.carry)+len(segment) > limit {
				// The record is already too long and its end has not arrived.
				// Count it now, release what was held, and discard the rest of
				// it as it streams in.
				b.skipped++
				b.dropping = true
				b.carry = nil
				return nil, false
			}
			b.carry = append(b.carry, segment...)
			return nil, false
		}
		segment := b.pending[:newline]
		b.pending = b.pending[newline+1:]
		b.records++
		if b.dropping {
			// The oversized record ends here and was counted when it overflowed.
			b.dropping = false
			continue
		}
		if len(b.carry)+len(segment) > limit {
			b.skipped++
			b.carry = nil
			continue
		}
		if len(b.carry) == 0 {
			return segment, true
		}
		line := append(b.carry, segment...)
		b.carry = nil
		return line, true
	}
	return nil, false
}
