package integrations

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestErrorRecorderResumesSequenceFromAppendOnlyLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "errors.jsonl")
	now := time.Date(2026, time.July, 16, 21, 0, 0, 0, time.UTC)
	first := NewErrorRecorder(path, "fixture-mac", func() time.Time { return now })
	for _, summary := range []string{"first", "second"} {
		if _, err := first.Emit(ErrorInput{Kind: "fixture", Summary: summary, Detail: summary + " detail"}); err != nil {
			t.Fatal(err)
		}
	}

	reopened := NewErrorRecorder(path, "fixture-mac", func() time.Time { return now.Add(time.Minute) })
	event, err := reopened.Emit(ErrorInput{Kind: "fixture", Summary: "third", Detail: "third detail"})
	if err != nil {
		t.Fatal(err)
	}
	if event.Seq != 3 {
		t.Fatalf("reopened event seq=%d, want 3", event.Seq)
	}
	feed, err := reopened.Feed(1)
	if err != nil {
		t.Fatal(err)
	}
	if feed.SchemaVersion != SchemaVersion || feed.NextSeq != 3 || len(feed.Errors) != 2 ||
		feed.Errors[0].Seq != 2 || feed.Errors[1].Seq != 3 {
		t.Fatalf("feed = %#v", feed)
	}
}

// A power cut leaves a half-written final record, and a bad block leaves
// undecodable bytes. Neither may disable error recording or the error feed.
func TestErrorRecorderSkipsTornRecordsAndKeepsRecording(t *testing.T) {
	path := filepath.Join(t.TempDir(), "errors.jsonl")
	torn := `{"seq":3,"ts":"2026-07-16T21:00:02Z","kind":"runner_exit","summ`
	log := strings.Join([]string{
		`{"seq":1,"ts":"2026-07-16T21:00:00Z","kind":"runner_exit","summary":"first","detail":"","machine":"fixture-mac"}`,
		"\xff\xfe\x00 not utf-8 and not json",
		`{"seq":2,"ts":"2026-07-16T21:00:01Z","kind":"runner_exit","summary":"second","detail":"","machine":"fixture-mac"}`,
		torn, // truncated mid-line, and deliberately without a trailing newline
	}, "\n")
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}

	recorder := NewErrorRecorder(path, "fixture-mac", func() time.Time {
		return time.Date(2026, time.July, 16, 21, 0, 10, 0, time.UTC)
	})
	feed, err := recorder.Feed(0)
	if err != nil {
		t.Fatalf("feed after torn write must still work: %v", err)
	}
	if len(feed.Errors) != 2 || feed.Errors[0].Seq != 1 || feed.Errors[1].Seq != 2 {
		t.Fatalf("feed = %#v", feed)
	}
	if feed.SkippedRecords != 2 {
		t.Fatalf("skipped records = %d, want 2 (invalid utf-8 line and torn tail)", feed.SkippedRecords)
	}

	event, err := recorder.Emit(ErrorInput{Kind: "runner_lost", Summary: "third"})
	if err != nil {
		t.Fatalf("recording must survive a torn log: %v", err)
	}
	if event.Seq != 3 {
		t.Fatalf("emitted seq = %d, want 3", event.Seq)
	}

	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(encoded), "\n"), "\n")
	if len(lines) != 5 || lines[3] != torn {
		t.Fatalf("torn bytes must be preserved, not rewritten:\n%s", encoded)
	}
	var appended ErrorEvent
	if err := json.Unmarshal([]byte(lines[4]), &appended); err != nil || appended.Summary != "third" {
		t.Fatalf("appended record was swallowed by the torn tail: %q (%v)", lines[4], err)
	}

	reopened := NewErrorRecorder(path, "fixture-mac", time.Now)
	reloaded, err := reopened.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Errors) != 3 || reloaded.NextSeq != 3 || reloaded.SkippedRecords != 2 {
		t.Fatalf("reloaded feed = %#v", reloaded)
	}
	t.Logf("feed survived: events=%d skipped=%d next_seq=%d", len(reloaded.Errors), reloaded.SkippedRecords, reloaded.NextSeq)
}

func TestErrorRecorderBoundsRetainedEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "errors.jsonl")
	var log strings.Builder
	total := maxRetainedErrorEvents + 100
	for seq := 1; seq <= total; seq++ {
		fmt.Fprintf(&log,
			`{"seq":%d,"ts":"2026-07-16T21:00:00Z","kind":"runner_exit","summary":"event %d","detail":"","machine":"fixture-mac"}`+"\n",
			seq, seq)
	}
	if err := os.WriteFile(path, []byte(log.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	recorder := NewErrorRecorder(path, "fixture-mac", time.Now)
	feed, err := recorder.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed.Errors) != maxRetainedErrorEvents {
		t.Fatalf("retained events = %d, want %d", len(feed.Errors), maxRetainedErrorEvents)
	}
	if feed.TruncatedBefore != uint64(total-maxRetainedErrorEvents+1) {
		t.Fatalf("truncated_before = %d, want %d", feed.TruncatedBefore, total-maxRetainedErrorEvents+1)
	}
	if _, err := recorder.Emit(ErrorInput{Kind: "runner_exit", Summary: "one more"}); err != nil {
		t.Fatal(err)
	}
	after, err := recorder.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Errors) != maxRetainedErrorEvents || after.NextSeq != uint64(total+1) {
		t.Fatalf("after emit: retained=%d next_seq=%d", len(after.Errors), after.NextSeq)
	}
}
