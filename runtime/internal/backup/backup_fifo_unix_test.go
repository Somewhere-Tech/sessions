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
	"time"
)

// A live transcript is the routine cause of a skipped session, so the reason
// has to read as a retry rather than a failure.
func TestReadStableFileReportsAGrowingTranscriptCalmly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.jsonl")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skip("mkfifo is unavailable: " + err.Error())
	}

	// readStableFile opens the FIFO once per attempt, so a writer has to be
	// there for each one. This serves writers in a loop rather than a fixed
	// count: an earlier version opened exactly twice and deadlocked whenever
	// the writer won the race for the first open, because the reader then saw
	// an empty FIFO, accepted it, and left the second open with nobody to pair
	// with. O_NONBLOCK is what makes that impossible now -- opening a FIFO for
	// writing with no reader fails immediately instead of parking forever, so
	// this goroutine always notices stop.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			writer, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				time.Sleep(time.Millisecond)
				continue
			}
			_, _ = writer.WriteString("{\"type\":\"user\"}\n")
			_ = writer.Close()
		}
	}()

	_, _, err := readStableFile(path)
	close(stop)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the writer goroutine never stopped; readStableFile is holding the FIFO open")
	}

	if err == nil {
		t.Fatal("readStableFile accepted an unstable read")
	}
	if !strings.Contains(err.Error(), "transcript changed while reading") ||
		!strings.Contains(err.Error(), "the next push picks it up") {
		t.Fatalf("err = %v, want a calm, instructional reason", err)
	}
}
