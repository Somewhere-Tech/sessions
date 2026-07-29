package state

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

const (
	SoftCapBytes         = 16 * 1024 * 1024
	TargetAfterTrimBytes = 8 * 1024 * 1024
	recordHeaderBytes    = 4
	recordFixedBytes     = 4
)

// PersistentLog is the append-only .events stream. Each record is
// [uint32 BE record length][uint32 BE seq][UTF-8 terminal data].
type PersistentLog struct {
	mu          sync.Mutex
	path        string
	file        *os.File
	bytesOnDisk int64
	closed      bool
}

func OpenPersistent(path string) (*PersistentLog, error) {
	_ = os.Remove(path + ".tmp")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &PersistentLog{path: path, file: f, bytesOnDisk: st.Size()}, nil
}

func Restore(path string) ([]Event, error) {
	buf, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Event
	off := 0
	for off+recordHeaderBytes <= len(buf) {
		recordLen := int(binary.BigEndian.Uint32(buf[off : off+recordHeaderBytes]))
		off += recordHeaderBytes
		if recordLen < recordFixedBytes || recordLen > len(buf)-off {
			break
		}
		body := buf[off : off+recordLen]
		out = append(out, Event{
			Seq:  binary.BigEndian.Uint32(body[:4]),
			Data: append([]byte(nil), body[4:]...),
		})
		off += recordLen
	}
	return out, nil
}

func (p *PersistentLog) Append(seq uint32, data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	recordLen := recordFixedBytes + len(data)
	frame := make([]byte, recordHeaderBytes+recordLen)
	binary.BigEndian.PutUint32(frame[:4], uint32(recordLen))
	binary.BigEndian.PutUint32(frame[4:8], seq)
	copy(frame[8:], data)
	if err := writeAll(p.file, frame); err != nil {
		return err
	}
	p.bytesOnDisk += int64(len(frame))
	if p.bytesOnDisk > SoftCapBytes {
		return p.trimLocked()
	}
	return nil
}

// Sync makes every complete appended event durable before a lifecycle
// boundary is published. Callers still serialize output and EXIT ordering;
// this method only owns the file-system durability boundary.
func (p *PersistentLog) Sync() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	return p.file.Sync()
}

func (p *PersistentLog) trimLocked() error {
	all, err := os.ReadFile(p.path)
	if err != nil {
		return err
	}
	offsets := make([]int, 0)
	off := 0
	for off+recordHeaderBytes <= len(all) {
		recordLen := int(binary.BigEndian.Uint32(all[off : off+4]))
		if recordLen < recordFixedBytes || off+recordHeaderBytes+recordLen > len(all) {
			break
		}
		offsets = append(offsets, off)
		off += recordHeaderBytes + recordLen
	}
	totalEnd := off
	firstKept := -1
	for _, candidate := range offsets {
		if totalEnd-candidate <= TargetAfterTrimBytes {
			firstKept = candidate
			break
		}
	}
	if firstKept < 0 {
		if len(offsets) > 0 {
			firstKept = offsets[len(offsets)-1]
		} else {
			firstKept = 0
		}
	}
	trimmed := all[firstKept:totalEnd]
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, trimmed, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p.path); err != nil {
		return err
	}
	_ = p.file.Close()
	p.file, err = os.OpenFile(p.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		p.closed = true
		return err
	}
	p.bytesOnDisk = int64(len(trimmed))
	return nil
}

func (p *PersistentLog) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	return p.file.Close()
}

func (p *PersistentLog) Unlink() error {
	closeErr := p.Close()
	removeErr := os.Remove(p.path)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(closeErr, removeErr)
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

func (p *PersistentLog) String() string {
	return fmt.Sprintf("PersistentLog(%s)", p.path)
}
