package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// A structured history file is one JSON record per line, appended in a single
// Write each. A Write that stops partway leaves an unterminated fragment, and
// the next record appended after it would join that line; the tail reader then
// skips the whole invalid line and the next record is lost with the fragment.
//
// Appending a newline before the next record isolates the fragment without
// touching it: no byte already on disk is rewritten, truncated or removed. A
// valid record that merely lacks its newline stays a valid line of its own.
// This prevents losing the NEXT record; it does not recover the bytes the
// failed Write did not store, make any record durable against power loss
// (nothing here syncs), or fix the condition that made the Write fail.

// structuredLogFile is the part of the history file the append boundary uses.
// *os.File satisfies it; tests inject a small fake.
type structuredLogFile interface {
	io.Writer
	io.ReaderAt
	Stat() (os.FileInfo, error)
}

// structuredLogEnd is what the runner knows about the last byte of its history
// file. The zero value means "not known yet": the first append reads the last
// byte once, so a file opened by any path, including a cold reopen of a log a
// previous runtime left unterminated, is handled the same way. Runners reset it
// whenever they open a history file.
type structuredLogEnd struct {
	known bool
	// open reports that the last byte is not a newline: a record, or a
	// fragment of one, is unterminated.
	open bool
}

// Each failed append wraps one of these and the Write's own error, so a
// caller can tell what the reported byte count says without parsing text. None
// of them claims more than the count does: an error does not prove that no
// bytes were stored, and a full count does not prove the bytes are durable.
var (
	// errStructuredLineOpen: the separator that would isolate an earlier
	// fragment was not stored, so this record was not appended onto the
	// damaged line. The next append tries the separator again.
	errStructuredLineOpen = errors.New("structured history line is still open: the separating newline was not stored, so this record was not written to disk")
	// errStructuredSeparatorError: the separating newline was reported stored
	// but its Write returned an error. The line counts as closed, so the next
	// record needs no separator; this record was not written.
	errStructuredSeparatorError = errors.New("the separating newline was reported stored but its write returned an error, so this record was not written to disk")
	// errStructuredRecordNotStored: the record's Write reported no bytes.
	errStructuredRecordNotStored = errors.New("structured history record write reported no bytes stored")
	// errStructuredRecordPartial: the record's Write reported some bytes; the
	// fragment stays as written and the next record starts on a new line.
	errStructuredRecordPartial = errors.New("structured history record write reported only part of its bytes stored")
	// errStructuredRecordStoredWithError: the record's Write reported every
	// byte stored and also returned an error. It is not written again; whether
	// the bytes are durable is unknown.
	errStructuredRecordStoredWithError = errors.New("structured history record write reported every byte stored but returned an error")
)

// appendStructuredRecord appends record as one line. It never writes a record
// a second time, whatever its Write reported: a second copy could duplicate a
// message or receipt. The caller logs the returned error and continues exactly
// as before; the record is still kept in memory and sent to clients.
func appendStructuredRecord(file structuredLogFile, end *structuredLogEnd, record []byte) error {
	if !end.known {
		end.open = structuredLogEndsOpen(file)
		end.known = true
	}
	if end.open {
		n, err := file.Write([]byte{'\n'})
		if n < 1 {
			if err == nil {
				err = io.ErrShortWrite
			}
			return fmt.Errorf("%w: %w", errStructuredLineOpen, err)
		}
		end.open = false
		if err != nil {
			return fmt.Errorf("%w: %w", errStructuredSeparatorError, err)
		}
	}
	line := make([]byte, 0, len(record)+1)
	line = append(append(line, record...), '\n')
	n, err := file.Write(line)
	stored := min(max(n, 0), len(line))
	if err == nil && stored < len(line) {
		err = io.ErrShortWrite
	}
	if stored > 0 {
		end.open = line[stored-1] != '\n'
	}
	switch {
	case err == nil:
		return nil
	case stored == 0:
		return fmt.Errorf("%w (0 of %d bytes): %w", errStructuredRecordNotStored, len(line), err)
	case stored < len(line):
		return fmt.Errorf("%w (%d of %d bytes); the fragment is left as written and the next record starts on a new line: %w", errStructuredRecordPartial, stored, len(line), err)
	}
	return fmt.Errorf("%w (%d of %d bytes); it is not written again and may not be durable: %w", errStructuredRecordStoredWithError, stored, len(line), err)
}

// structuredLogEndsOpen reads the last byte once. When it cannot tell, it
// answers open: an extra blank line is skipped by every reader, while a missing
// separator would cost the next record.
func structuredLogEndsOpen(file structuredLogFile) bool {
	info, err := file.Stat()
	if err != nil {
		return true
	}
	if info.Size() == 0 {
		return false
	}
	var last [1]byte
	if n, _ := file.ReadAt(last[:], info.Size()-1); n != 1 {
		return true
	}
	return last[0] != '\n'
}
