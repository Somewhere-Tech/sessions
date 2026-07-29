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
