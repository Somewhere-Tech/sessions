package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// A failed open used to be cached forever by a sync.Once, so one bad moment
// disabled usage recording for the whole process with no way back.
func TestFailedOpenDoesNotDisableTheLedgerForever(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewService(Options{Path: filepath.Join(blocked, "usage.sqlite3")})
	defer service.Close()
	if _, err := service.database(context.Background()); err == nil {
		t.Fatal("opening a ledger under a regular file unexpectedly succeeded")
	}
	if _, err := service.Report(context.Background(), ReportOptions{Group: "daily"}); err == nil {
		t.Fatal("reporting against an unopenable ledger unexpectedly succeeded")
	}

	service.options.Path = filepath.Join(root, "usage.sqlite3")
	db, err := service.database(context.Background())
	if err != nil {
		t.Fatalf("ledger stayed disabled after the fault cleared: %v", err)
	}
	if db == nil {
		t.Fatal("recovered open returned no database")
	}
	if err := service.RecordStructured(context.Background(),
		state.SessionInfo{ID: "sessions-claude", Tool: state.ToolClaude, ClaudeSessionID: "claude-live"},
		json.RawMessage(`{"timestamp":"2026-07-20T08:00:00Z","session_id":"claude-live","message":{"id":"message-live","model":"claude-sonnet-4-6","usage":{"input_tokens":100,"output_tokens":20}}}`)); err != nil {
		t.Fatalf("usage recording stayed dead after recovery: %v", err)
	}
}

// A migration must never guess at the table shape. An I/O error part way
// through PRAGMA table_info used to be dropped on the floor, which left the
// column looking absent, ran the ALTER TABLE blind, failed it with "duplicate
// column name", and cached that failure as the ledger's permanent state.
func TestEnsureColumnRefusesToGuessWhenTheShapeCannotBeRead(t *testing.T) {
	db, ledger := openFaultyLedger(t)
	err := ensureColumn(context.Background(), db, "usage_entries", "reasoning_tokens", "INTEGER NOT NULL DEFAULT 0")
	if err == nil {
		t.Fatal("an interrupted table_info read was treated as a missing column")
	}
	if !strings.Contains(err.Error(), faultyReadMessage) {
		t.Fatalf("migration error = %v, want the underlying read failure %q", err, faultyReadMessage)
	}
	for _, statement := range faulty.executed(ledger) {
		if strings.Contains(strings.ToUpper(statement), "ALTER TABLE") {
			t.Fatalf("a column was added despite the failed shape read: %q", statement)
		}
	}
}

func TestEnsureColumnRefusesToGuessWhenTheShapeCannotBeQueried(t *testing.T) {
	db := openLedgerTable(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ensureColumn(cancelled, db, "usage_entries", "reasoning_tokens", "INTEGER NOT NULL DEFAULT 0"); err == nil {
		t.Fatal("an unreadable table shape was treated as a missing column")
	}
	found, err := columnExists(context.Background(), db, "usage_entries", "reasoning_tokens")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("a column was added despite the failed shape read")
	}
}

// The reverse of the same defence: if the column does appear between the read
// and the ALTER, the wanted outcome already holds and must not be an error.
func TestAddColumnToleratesAColumnThatAlreadyExists(t *testing.T) {
	db := openLedgerTable(t)
	ctx := context.Background()
	if err := addColumn(ctx, db, "usage_entries", "reasoning_tokens", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		t.Fatal(err)
	}
	if err := addColumn(ctx, db, "usage_entries", "reasoning_tokens", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		t.Fatalf("a duplicate column was reported as a failure: %v", err)
	}
	if err := addColumn(ctx, db, "usage_absent", "reasoning_tokens", "INTEGER"); err == nil {
		t.Fatal("a genuinely broken migration was swallowed")
	}
}

// Close used to read s.db with neither the mutex nor the once that writes it.
func TestCloseIsSafeAlongsideConcurrentUse(t *testing.T) {
	service := NewService(Options{Path: filepath.Join(t.TempDir(), "usage.sqlite3")})
	start := make(chan struct{})
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			if _, err := service.database(context.Background()); err != nil &&
				!strings.Contains(err.Error(), "closed") {
				t.Error(err)
			}
		}()
	}
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			if err := service.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	group.Wait()
	if err := service.Close(); err != nil {
		t.Fatalf("second close = %v", err)
	}
}

func openLedgerTable(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "usage.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE usage_entries (event_key TEXT PRIMARY KEY, model TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	return db
}

// A corrupt token count must not become a cost. int64(float64) turned 1e30
// into a large negative count and a wildly negative price.
func TestTokenCountRejectsImplausibleNumbers(t *testing.T) {
	tests := []struct {
		name  string
		value float64
		want  int64
	}{
		{name: "plain count", value: 1024, want: 1024},
		{name: "zero", value: 0},
		{name: "fractional counts truncate", value: 1024.9, want: 1024},
		{name: "largest plausible count", value: float64(maxPlausibleTokens), want: maxPlausibleTokens},
		{name: "corrupt exponent", value: 1e30},
		{name: "beyond int64", value: 1e300},
		{name: "negative", value: -5},
		{name: "negative corrupt exponent", value: -1e30},
		{name: "not a number", value: math.NaN()},
		{name: "infinite", value: math.Inf(1)},
		{name: "negative infinite", value: math.Inf(-1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := tokenCount(test.value); got != test.want {
				t.Fatalf("tokenCount(%v) = %d, want %d", test.value, got, test.want)
			}
		})
	}
}

// The same protection at the parse boundary: a corrupt provider line is
// dropped rather than turned into an invented cost.
func TestCorruptTokenCountsNeverBecomeCost(t *testing.T) {
	stamp := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)
	corrupt := []string{
		`{"timestamp":"2026-07-20T08:00:00Z","sessionId":"claude-session","message":{"id":"message-1","model":"claude-sonnet-4-6","usage":{"input_tokens":1e30,"output_tokens":-4}}}`,
		`{"timestamp":"2026-07-20T08:00:00Z","sessionId":"claude-session","message":{"id":"message-2","model":"claude-sonnet-4-6","usage":{"input_tokens":-1e30}}}`,
	}
	for _, line := range corrupt {
		if parsed := parseClaudeLine("/tmp/claude.jsonl", 0, []byte(line), stamp); parsed != nil {
			t.Fatalf("corrupt usage became an entry: tokens %#v cost %.6f", parsed.tokens, parsed.calculated)
		}
	}
	mixed := `{"timestamp":"2026-07-20T08:00:00Z","sessionId":"claude-session","message":{"id":"message-3","model":"claude-sonnet-4-6","usage":{"input_tokens":1e30,"output_tokens":20}}}`
	parsed := parseClaudeLine("/tmp/claude.jsonl", 0, []byte(mixed), stamp)
	if parsed == nil {
		t.Fatal("a line with one good count was dropped entirely")
	}
	if parsed.tokens.Input != 0 || parsed.tokens.Output != 20 || parsed.calculated <= 0 {
		t.Fatalf("corrupt field leaked into the entry: tokens %#v cost %.12f", parsed.tokens, parsed.calculated)
	}
}
