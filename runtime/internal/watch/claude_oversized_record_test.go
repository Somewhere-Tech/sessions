package watch

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A transcript containing a record longer than the bound must keep streaming the
// records around it, must not hold the record in memory, and must report the
// skip -- the torn-record policy in internal/integrations/errors.go: skipped,
// counted, and reachable by the caller, never silently dropped.
func TestClaudeWatcherSkipsAndCountsAnOversizedRecord(t *testing.T) {
	projectDir := t.TempDir()
	sessionID := "aaaaaaaa-1111-2222-3333-555555555555"
	path := filepath.Join(projectDir, sessionID+".jsonl")

	before := SessionEvent{"type": "user", "uuid": "before-the-giant"}
	after := SessionEvent{"type": "assistant", "uuid": "after-the-giant"}

	const recordBound = 8 << 10
	// One record two hundred times the bound, valid JSON, with no newline until
	// it ends. Unbounded, this is the shape that makes the tail hold the whole
	// file: the real instance on this machine is a 1.15 GB transcript.
	giant := append([]byte(`{"type":"user","uuid":"giant","text":"`), bytes.Repeat([]byte{'g'}, 200*recordBound)...)
	giant = append(giant, []byte(`"}`)...)

	var file bytes.Buffer
	file.Write(sessionEventBytes(t, []SessionEvent{before}))
	file.Write(giant)
	file.WriteByte('\n')
	file.Write(sessionEventBytes(t, []SessionEvent{after}))
	if err := os.WriteFile(path, file.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	mirrorPath := filepath.Join(t.TempDir(), "session.transcript.jsonl")
	watcher, err := WatchClaudeSession(ClaudeWatcherOptions{
		ClaudeSessionID: sessionID,
		ProjectDir:      projectDir,
		InitialDelay:    time.Millisecond,
		PollInterval:    10 * time.Millisecond,
		MaxRecordBytes:  recordBound,
		MirrorPath:      mirrorPath,
		SessionID:       "fixture-session",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()

	// The records on either side of the oversized one still arrive, in order.
	got := collectEvents(t, watcher.Events, 2, 2*time.Second)
	assertEventsJSONEqual(t, got, []SessionEvent{before, after})
	assertNoEvent(t, watcher.Events, 80*time.Millisecond)

	if skipped := watcher.SkippedRecords(); skipped != 1 {
		t.Fatalf("SkippedRecords = %d, want 1", skipped)
	}
	select {
	case err := <-watcher.Errors:
		if err == nil || !strings.Contains(err.Error(), "skipped a record longer than") {
			t.Fatalf("watcher error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("no error surfaced for the skipped record")
	}

	// The provider file is never repaired or truncated: the oversized bytes stay
	// on disk for a later forensic read.
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, file.Bytes()) {
		t.Fatal("the watcher modified the provider transcript")
	}

	// The mirror stores what the tail forwarded and nothing it could not read.
	watcher.Close()
	mirrored, err := os.ReadFile(mirrorPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(mirrored, []byte(`"uuid":"giant"`)) {
		t.Fatal("mirror stored a record the tail skipped")
	}
	for _, uuid := range []string{"before-the-giant", "after-the-giant"} {
		if !bytes.Contains(mirrored, []byte(uuid)) {
			t.Fatalf("mirror is missing %s", uuid)
		}
	}
}

// The default bound is the one production uses, and it must be able to hold the
// largest record either provider has ever actually written here: 16,917,872
// bytes, in a Codex rollout. A bound below that would skip real conversation.
func TestDefaultRecordBoundClearsTheLargestObservedRecord(t *testing.T) {
	const largestObservedRecordBytes = 16_917_872
	if maxTranscriptRecordBytes <= largestObservedRecordBytes {
		t.Fatalf("record bound %d does not clear the largest observed record %d",
			maxTranscriptRecordBytes, largestObservedRecordBytes)
	}
	// A record the tail accepts must be one the mirror's own reader can scan
	// back, or the mirror forgets it on the next open.
	if maxTranscriptRecordBytes > transcriptScanLineCap {
		t.Fatalf("record bound %d exceeds the mirror scan cap %d",
			maxTranscriptRecordBytes, transcriptScanLineCap)
	}
	var buffer lineBuffer
	if buffer.limit() != maxTranscriptRecordBytes {
		t.Fatalf("zero-value limit = %d, want %d", buffer.limit(), maxTranscriptRecordBytes)
	}
}
