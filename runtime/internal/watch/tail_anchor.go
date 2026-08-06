package watch

import (
	"bytes"
	"os"
)

// A tail resumes reading a provider transcript at the byte offset it stopped
// at. Deciding that the offset is still meaningful is the whole correctness
// problem, and file identity plus size does not decide it.
//
// os.SameFile catches a replaced file, and a size below the offset catches a
// truncation. Neither catches the case that actually loses data: the provider
// rewrites its transcript in place, on the same inode, to a size at or above
// the old offset. Every stat-derived signal still says "same file, only grown",
// so the tail resumes mid-record in a byte stream that is no longer the one it
// was reading. What it parses from there is not the new content and not the old
// content; it is the tail of a record that no longer exists, and everything
// before the offset is never read at all.
//
// The transcript mirror limits the damage -- it holds the union of what it has
// already stored -- but it cannot repair it, because the records it never sees
// are the ones the rewrite moved to before the offset.
//
// A readAnchor closes it with the only evidence that actually settles the
// question: the bytes themselves. The tail remembers the last claudeAnchorBytes
// it consumed, and before resuming it re-reads that same span from the file. If
// those bytes still stand, the stream is continuous by direct observation and
// the offset is valid. If they do not, the file was rewritten underneath the
// tail whatever its inode and size say, and the tail restarts from zero.
const claudeAnchorBytes = 512

type readAnchor struct {
	bytes []byte
}

// reset drops the anchor. Used whenever the tail restarts at offset zero, where
// there is no preceding content to anchor to.
func (a *readAnchor) reset() {
	a.bytes = a.bytes[:0]
}

// advance folds a freshly consumed chunk into the anchor, keeping the trailing
// claudeAnchorBytes. Chunks smaller than the window are accumulated rather than
// replacing it, so a transcript that arrives one short line at a time still ends
// up with a full-width anchor.
func (a *readAnchor) advance(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	if len(chunk) >= claudeAnchorBytes {
		a.bytes = append(a.bytes[:0], chunk[len(chunk)-claudeAnchorBytes:]...)
		return
	}
	a.bytes = append(a.bytes, chunk...)
	if len(a.bytes) > claudeAnchorBytes {
		a.bytes = append(a.bytes[:0], a.bytes[len(a.bytes)-claudeAnchorBytes:]...)
	}
}

// intact reports whether the bytes immediately before offset still match what
// the tail consumed. An offset of zero, or an empty anchor, is vacuously intact:
// there is nothing to contradict.
//
// A short or failed read reports NOT intact. That biases toward re-reading the
// file from the beginning, which costs a duplicate pass that the mirror and the
// tail's own emitted-uuid set both absorb. The opposite bias would silently drop
// conversation, and this whole file exists because that is the expensive error.
func (a *readAnchor) intact(file *os.File, offset int64) bool {
	if offset <= 0 || len(a.bytes) == 0 {
		return true
	}
	width := int64(len(a.bytes))
	if width > offset {
		// More anchor than file precedes the offset: the file cannot be the one
		// those bytes came from.
		return false
	}
	current, err := readRange(file, offset-width, width)
	if err != nil || int64(len(current)) != width {
		return false
	}
	return bytes.Equal(current, a.bytes)
}
