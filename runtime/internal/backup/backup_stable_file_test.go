package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Grow the real file after the reader's first stat, not according to scheduler
// timing. Each new open grows once, so both bounded attempts are unstable.
type growingTranscriptFile struct {
	*os.File
	path  string
	grown bool
}

func (f *growingTranscriptFile) Read(bytes []byte) (int, error) {
	if !f.grown {
		writer, err := os.OpenFile(f.path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			return 0, err
		}
		_, err = writer.WriteString("{\"type\":\"assistant\"}\n")
		closeErr := writer.Close()
		if err != nil {
			return 0, err
		}
		if closeErr != nil {
			return 0, closeErr
		}
		f.grown = true
	}
	return f.File.Read(bytes)
}

// A live transcript is routine, so its retry reason must remain calm. A FIFO
// cannot deterministically represent regular-file growth on every Unix kernel.
func TestReadStableFileReportsAGrowingTranscriptCalmly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"user\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	_, _, err := readStableFileWithOpener(path, func(path string) (stableTranscriptFile, error) {
		attempts++
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		return &growingTranscriptFile{File: file, path: path}, nil
	})
	if attempts != 2 {
		t.Fatalf("read attempts = %d, want exactly two", attempts)
	}
	if err == nil {
		t.Fatal("readStableFile accepted an unstable read")
	}
	if !strings.Contains(err.Error(), "transcript changed while reading") ||
		!strings.Contains(err.Error(), "the next push picks it up") {
		t.Fatalf("err = %v, want a calm, instructional reason", err)
	}
}
