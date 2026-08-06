package verdict

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
)

func TestDecodeRejectsJunk(t *testing.T) {
	tests := map[string]string{
		"empty":           ``,
		"not object":      `[]`,
		"missing version": `{"verdict":"pass"}`,
		"wrong version":   `{"schemaVersion":2,"verdict":"pass"}`,
		"empty verdict":   `{"schemaVersion":1,"verdict":" "}`,
		"unknown field":   `{"schemaVersion":1,"verdict":"pass","content":"not allowed"}`,
		"duplicate field": `{"schemaVersion":1,"verdict":"pass","verdict":"fail"}`,
		"null findings":   `{"schemaVersion":1,"verdict":"pass","findings":null}`,
		"bad finding":     `{"schemaVersion":1,"verdict":"pass","findings":[{"severity":"error"}]}`,
		"finding junk":    `{"schemaVersion":1,"verdict":"pass","findings":[{"severity":"error","title":"x","extra":true}]}`,
		"bad line":        `{"schemaVersion":1,"verdict":"pass","findings":[{"severity":"error","title":"x","line":0}]}`,
		"null detail":     `{"schemaVersion":1,"verdict":"pass","findings":[{"severity":"error","title":"x","detail":null}]}`,
		"meta array":      `{"schemaVersion":1,"verdict":"pass","meta":[]}`,
		"meta null":       `{"schemaVersion":1,"verdict":"pass","meta":null}`,
		"trailing":        `{"schemaVersion":1,"verdict":"pass"} {}`,
	}
	for name, encoded := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(strings.NewReader(encoded)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Decode error = %v, want ErrInvalid", err)
			}
		})
	}

	document, err := Decode(strings.NewReader(`{"schemaVersion":1,"verdict":"needs-review","findings":[{"severity":"warning","title":"check this","file":"main.go","line":7}],"meta":{"producer":"test"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if document.Verdict != "needs-review" || len(document.Findings) != 1 || document.Meta["producer"] != "test" {
		t.Fatalf("decoded document = %#v", document)
	}
}

func TestLatestWinsAcrossThreeEmitsAndLedgerContainsOnlyPointers(t *testing.T) {
	root := t.TempDir()
	ledgerPath := filepath.Join(root, "ledger", "lanes.sqlite3")
	ledgerStore, err := ledger.Open(context.Background(), ledger.Options{Path: ledgerPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledgerStore.Close() })

	tick := 0
	store, err := NewStore(Options{
		StateDir: filepath.Join(root, "runners"), LedgerPath: ledgerPath,
		Clock: func() time.Time {
			tick++
			return time.Date(2026, 7, 16, 20, 0, tick, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	for index, value := range []string{"blocked", "fail", "pass"} {
		record, err := store.Emit(context.Background(), id, Document{SchemaVersion: 1, Verdict: value})
		if err != nil {
			t.Fatal(err)
		}
		if record.Seq != uint64(index+1) {
			t.Fatalf("emit %d seq = %d", index+1, record.Seq)
		}
	}
	latest, err := store.Latest(id)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Seq != 3 || latest.Verdict != "pass" || latest.EmittedAt != "2026-07-16T20:00:03Z" {
		t.Fatalf("latest = %#v", latest)
	}
	path, _ := store.Path(id)
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimSpace(string(encoded)), "\n") + 1; lines != 3 {
		t.Fatalf("JSONL lines = %d\n%s", lines, encoded)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("verdict log mode = %#o", info.Mode().Perm())
	}

	events, err := ledgerStore.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("ledger events = %d, want 3", len(events))
	}
	for _, event := range events {
		if event.Type != ledger.EventType("verdict") || string(event.Payload) != "{}" {
			t.Fatalf("ledger event leaked verdict content: type=%q payload=%s", event.Type, event.Payload)
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil || len(payload) != 0 {
			t.Fatalf("ledger payload = %s, err=%v", event.Payload, err)
		}
	}
	t.Logf("latest seq=%d verdict=%s jsonl_lines=3 ledger_pointer_payload={}", latest.Seq, latest.Verdict)
}

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	ledgerPath := filepath.Join(root, "ledger", "lanes.sqlite3")
	ledgerStore, err := ledger.Open(context.Background(), ledger.Options{Path: ledgerPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledgerStore.Close() })
	tick := 0
	store, err := NewStore(Options{
		StateDir: filepath.Join(root, "runners"), LedgerPath: ledgerPath,
		Clock: func() time.Time {
			tick++
			return time.Date(2026, 8, 5, 9, 0, tick, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, ledgerPath
}

// A torn final record from a power cut must cost that record only: the lane's
// earlier verdicts stay readable and the next emit still lands.
func TestLatestSkipsTornRecordsAndReportsTheSkip(t *testing.T) {
	store, _ := newTestStore(t)
	id := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	for _, value := range []string{"blocked", "pass"} {
		if _, err := store.Emit(context.Background(), id, Document{SchemaVersion: 1, Verdict: value}); err != nil {
			t.Fatal(err)
		}
	}
	path, err := store.Path(id)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// Invalid UTF-8 garbage, then a record truncated mid-line with no newline.
	if _, err := file.WriteString("\xff\xfe\x00garbage\n" + `{"schemaVersion":1,"verdict":"fa`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	latest, err := store.Latest(id)
	if err != nil {
		t.Fatalf("one torn record must not hide every verdict: %v", err)
	}
	if latest.Verdict != "pass" || latest.Seq != 2 {
		t.Fatalf("latest = %#v", latest)
	}
	if latest.SkippedRecords != 2 {
		t.Fatalf("skipped records = %d, want 2", latest.SkippedRecords)
	}

	next, err := store.Emit(context.Background(), id, Document{SchemaVersion: 1, Verdict: "needs-review"})
	if err != nil {
		t.Fatalf("emit must survive a torn log: %v", err)
	}
	if next.Seq != 3 {
		t.Fatalf("emitted seq = %d, want 3", next.Seq)
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(encoded), "\n"), "\n")
	if len(lines) != 5 || lines[3] != `{"schemaVersion":1,"verdict":"fa` {
		t.Fatalf("torn bytes must be preserved, not rewritten:\n%s", encoded)
	}
	var appended Record
	if err := json.Unmarshal([]byte(lines[4]), &appended); err != nil || appended.Verdict != "needs-review" {
		t.Fatalf("appended record was swallowed by the torn tail: %q (%v)", lines[4], err)
	}
	if strings.Contains(lines[4], "skipped_records") {
		t.Fatalf("read-side diagnostic leaked into the persisted record: %s", lines[4])
	}
	after, err := store.Latest(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Verdict != "needs-review" || after.Seq != 3 || after.SkippedRecords != 2 {
		t.Fatalf("latest after repair-free append = %#v", after)
	}
	t.Logf("latest verdict=%s seq=%d skipped=%d", after.Verdict, after.Seq, after.SkippedRecords)
}

func TestLatestReportsUnreadableLogAsNotFoundWithCount(t *testing.T) {
	store, _ := newTestStore(t)
	id := "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"
	path, err := store.Path(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("\xff\xfe not json\n{\"schemaVersion\":1,\"verd"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = store.Latest(id)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Latest error = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "2 unreadable record(s)") || !strings.Contains(err.Error(), path) {
		t.Fatalf("error must name the file and the skip count: %v", err)
	}
}

// The ledger pointer is written before the durable record so a ledger failure
// leaves nothing behind and a retrying producer cannot append a duplicate.
func TestEmitWritesNoRecordWhenTheLedgerPointerFails(t *testing.T) {
	root := t.TempDir()
	ledgerPath := filepath.Join(root, "lanes.sqlite3")
	if err := os.WriteFile(ledgerPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(Options{StateDir: filepath.Join(root, "runners"), LedgerPath: ledgerPath})
	if err != nil {
		t.Fatal(err)
	}
	id := "cccccccc-dddd-4eee-8fff-000000000000"
	if _, err := store.Emit(context.Background(), id, Document{SchemaVersion: 1, Verdict: "pass"}); err == nil {
		t.Fatal("emit must fail when the ledger pointer cannot be recorded")
	}
	path, err := store.Path(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		encoded, _ := os.ReadFile(path)
		t.Fatalf("failed emit left a visible record a retry would duplicate: %s", encoded)
	}
}
