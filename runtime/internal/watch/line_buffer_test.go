package watch

import (
	"bytes"
	"testing"
)

// A record with no newline in sight must not grow the process. Before the
// bound, `buffer += string(chunk)` held every byte of the record and copied the
// whole accumulation on each chunk: quadratic work and full-file residency for
// the 1.15 GB rollout on this machine.
func TestLineBufferBoundsMemoryAndCountsTheSkippedRecord(t *testing.T) {
	const limit = 4096
	buffer := lineBuffer{max: limit}
	chunk := bytes.Repeat([]byte{'x'}, 1<<20)

	// A hundred megabytes of a single unterminated record.
	for pass := 0; pass < 100; pass++ {
		buffer.feed(chunk)
		for {
			if line, ok := buffer.next(); ok {
				t.Fatalf("unterminated record produced a line of %d bytes", len(line))
			} else {
				break
			}
		}
		if held := len(buffer.carry) + cap(buffer.carry); held > 2*limit {
			t.Fatalf("held %d bytes after pass %d, want at most the bound", held, pass)
		}
	}
	if buffer.skipped != 1 {
		t.Fatalf("skipped = %d, want the oversized record counted exactly once", buffer.skipped)
	}

	// The record ends and the stream recovers: the next record is delivered.
	buffer.feed([]byte("tail-of-the-huge-record\n{\"uuid\":\"after\"}\n"))
	line, ok := buffer.next()
	if !ok || string(line) != `{"uuid":"after"}` {
		t.Fatalf("recovery line = %q, ok=%v", line, ok)
	}
	if _, ok := buffer.next(); ok {
		t.Fatal("buffer produced a line past the end of the chunk")
	}
	if buffer.skipped != 1 {
		t.Fatalf("skipped = %d after recovery, want 1", buffer.skipped)
	}
	if buffer.records != 2 {
		t.Fatalf("records = %d, want the skipped record still counted by position", buffer.records)
	}
}

func TestLineBufferSplitsRecordsAcrossChunkBoundaries(t *testing.T) {
	buffer := lineBuffer{max: 64}
	feed := func(text string) []string {
		buffer.feed([]byte(text))
		var lines []string
		for {
			line, ok := buffer.next()
			if !ok {
				return lines
			}
			lines = append(lines, string(line))
		}
	}

	if got := feed("one\ntw"); len(got) != 1 || got[0] != "one" {
		t.Fatalf("first chunk = %q", got)
	}
	if got := feed("o\nthree\n"); len(got) != 2 || got[0] != "two" || got[1] != "three" {
		t.Fatalf("second chunk = %q", got)
	}
	// A UTF-8 codepoint split across chunks is reassembled byte for byte.
	if got := feed("h\xc3"); len(got) != 0 {
		t.Fatalf("partial codepoint produced %q", got)
	}
	if got := feed("\xa9llo\n"); len(got) != 1 || got[0] != "héllo" {
		t.Fatalf("rejoined = %q", got)
	}
	// An empty record is still a record: callers count positions with it.
	if got := feed("\n\n"); len(got) != 2 || got[0] != "" || got[1] != "" {
		t.Fatalf("blank records = %q", got)
	}
	if buffer.skipped != 0 {
		t.Fatalf("skipped = %d, want 0", buffer.skipped)
	}

	// A record exactly at the bound is kept; one byte more is skipped.
	atLimit := string(bytes.Repeat([]byte{'a'}, 64))
	if got := feed(atLimit + "\n"); len(got) != 1 || got[0] != atLimit {
		t.Fatalf("record at the bound = %d records", len(got))
	}
	if got := feed(atLimit + "b\nkept\n"); len(got) != 1 || got[0] != "kept" {
		t.Fatalf("record past the bound = %q, want only the following record", got)
	}
	if buffer.skipped != 1 {
		t.Fatalf("skipped = %d, want 1", buffer.skipped)
	}

	// reset returns to a fresh replay but keeps the running skip total, which is
	// reported as a lifetime number rather than a per-pass one.
	buffer.reset()
	if buffer.records != 0 || buffer.skipped != 1 || buffer.carry != nil {
		t.Fatalf("after reset: records=%d skipped=%d carry=%d", buffer.records, buffer.skipped, len(buffer.carry))
	}
}
