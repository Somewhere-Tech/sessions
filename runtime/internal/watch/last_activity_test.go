package watch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConversationRecordedActivityIgnoresFileModificationTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conversation.jsonl")
	contents := strings.Join([]string{
		`{"type":"user","timestamp":"2026-07-16T17:01:00Z","message":{"role":"user","content":"first"}}`,
		`{"type":"assistant","timestamp":"2026-07-16T17:01:02Z","message":{"role":"assistant","content":"last"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	// This is what a `cp -R` does: the conversation is months old and the file
	// claims it happened just now.
	copied := time.Date(2026, time.December, 25, 9, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, copied, copied); err != nil {
		t.Fatal(err)
	}

	got, ok := ConversationRecordedActivity(path)
	if !ok {
		t.Fatal("no recorded activity found in a transcript that stamped every record")
	}
	want := time.Date(2026, time.July, 16, 17, 1, 2, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("recorded activity = %s, want %s", got, want)
	}
}

// A torn final record from a power cut must not make a conversation look older
// than the record before it, so the window's maximum is taken rather than its
// last line.
func TestConversationRecordedActivitySurvivesATornFinalRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "torn.jsonl")
	contents := `{"type":"user","timestamp":"2026-07-16T17:01:00Z"}` + "\n" +
		`{"type":"assistant","timestamp":"2026-07-16T17:05:00Z"}` + "\n" +
		`{"type":"assistant","timestamp":"2026-07-1`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok := ConversationRecordedActivity(path)
	if !ok || !got.Equal(time.Date(2026, time.July, 16, 17, 5, 0, 0, time.UTC)) {
		t.Fatalf("recorded activity = %s ok=%v", got, ok)
	}
}

// The read is a bounded tail, which is what keeps it affordable on the 1.1 GB
// transcript on this machine. A record that begins before the window opens is
// not a record and must not be parsed as one.
func TestConversationRecordedActivityReadsOnlyABoundedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.jsonl")
	var builder strings.Builder
	builder.WriteString(`{"type":"user","timestamp":"2020-01-01T00:00:00Z","text":"` +
		strings.Repeat("padding ", 20_000) + `"}` + "\n")
	builder.WriteString(`{"type":"assistant","timestamp":"2026-08-05T13:33:15Z"}` + "\n")
	if err := os.WriteFile(path, []byte(builder.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() <= conversationTailBytes {
		t.Fatalf("fixture is not larger than the tail window: size=%v err=%v", info.Size(), err)
	}
	got, ok := ConversationRecordedActivity(path)
	if !ok || !got.Equal(time.Date(2026, time.August, 5, 13, 33, 15, 0, time.UTC)) {
		t.Fatalf("recorded activity = %s ok=%v", got, ok)
	}
}

func TestConversationRecordedActivityRefusesWhenNothingWasStamped(t *testing.T) {
	dir := t.TempDir()
	// The real case: a 149-byte Claude bridge-session record with no timestamp.
	path := filepath.Join(dir, "bridge.jsonl")
	if err := os.WriteFile(path,
		[]byte(`{"type":"bridge-session","sessionId":"f3820efd","lastSequenceNum":1611}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := ConversationRecordedActivity(path); ok {
		t.Fatal("a transcript that stamped nothing reported a recorded activity time")
	}
	if _, ok := ConversationRecordedActivity(filepath.Join(dir, "absent.jsonl")); ok {
		t.Fatal("a missing file reported a recorded activity time")
	}
	empty := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := ConversationRecordedActivity(empty); ok {
		t.Fatal("an empty file reported a recorded activity time")
	}
}

func TestConversationRecordedActivityReadsEpochTimestamps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "epoch.jsonl")
	if err := os.WriteFile(path,
		[]byte(`{"type":"user","timestamp":1784221263000}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok := ConversationRecordedActivity(path)
	if !ok || got.UnixMilli() != 1784221263000 {
		t.Fatalf("recorded activity = %s ok=%v", got, ok)
	}
}
