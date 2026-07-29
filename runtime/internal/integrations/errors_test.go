package integrations

import (
	"path/filepath"
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
