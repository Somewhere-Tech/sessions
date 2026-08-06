//go:build !windows

// The growing-transcript case needs a file whose reads block until a writer
// appears, which is a FIFO here. Windows has no equivalent, and a runtime skip
// is not enough: syscall.Mkfifo does not exist there, so the test file has to
// leave the Windows build entirely rather than compile and skip.
package backup

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A live transcript is the routine cause of a skipped session, so the reason
// has to read as a retry rather than a failure.
func TestReadStableFileReportsAGrowingTranscriptCalmly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.jsonl")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skip("mkfifo is unavailable: " + err.Error())
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// readStableFile makes two attempts; feed both.
		for attempt := 0; attempt < 2; attempt++ {
			writer, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				return
			}
			_, _ = writer.WriteString("{\"type\":\"user\"}\n")
			_ = writer.Close()
		}
	}()
	_, _, err := readStableFile(path)
	<-done
	if err == nil {
		t.Fatal("readStableFile accepted an unstable read")
	}
	if !strings.Contains(err.Error(), "transcript changed while reading") ||
		!strings.Contains(err.Error(), "the next push picks it up") {
		t.Fatalf("err = %v, want a calm, instructional reason", err)
	}
}
