package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Service struct {
	options Options
	// initMu guards the lazy open below. It is deliberately not the same lock
	// as mu: Sync and RecordStructured hold mu across a call to database.
	initMu sync.Mutex
	db     *sql.DB
	closed bool
	mu     sync.Mutex
}

func NewService(options Options) *Service {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Machine == "" {
		options.Machine, _ = os.Hostname()
		if options.Machine == "" {
			options.Machine = "this-machine"
		}
	}
	return &Service{options: options}
}

// database opens the ledger on first use. A failed attempt is never cached:
// a transient fault (a busy disk, a half-written PRAGMA read) must not disable
// usage recording for the rest of the process lifetime, so the next caller
// retries from scratch.
func (s *Service) database(ctx context.Context) (*sql.DB, error) {
	s.initMu.Lock()
	defer s.initMu.Unlock()
	if s.closed {
		return nil, errors.New("usage service is closed")
	}
	if s.db != nil {
		return s.db, nil
	}
	db, err := s.openDatabase(ctx)
	if err != nil {
		return nil, err
	}
	s.db = db
	return s.db, nil
}

func (s *Service) openDatabase(ctx context.Context) (*sql.DB, error) {
	if s.options.Path == "" {
		return nil, errors.New("usage database path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(s.options.Path), 0o700); err != nil {
		return nil, fmt.Errorf("create usage state directory: %w", err)
	}
	db, err := sql.Open("sqlite", s.options.Path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, `
PRAGMA journal_mode=WAL;
PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS usage_sources (
  path TEXT PRIMARY KEY,
  provider TEXT NOT NULL,
  offset_bytes INTEGER NOT NULL DEFAULT 0,
  size_bytes INTEGER NOT NULL DEFAULT 0,
  mtime_ns INTEGER NOT NULL DEFAULT 0,
  parser_state TEXT NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS usage_entries (
  event_key TEXT PRIMARY KEY,
  source_path TEXT NOT NULL,
  source_offset INTEGER NOT NULL,
  provider TEXT NOT NULL,
  provider_session_id TEXT NOT NULL,
  timestamp_ms INTEGER NOT NULL,
  model TEXT NOT NULL,
  input_tokens INTEGER NOT NULL,
  output_tokens INTEGER NOT NULL,
  cache_creation_tokens INTEGER NOT NULL,
  cache_read_tokens INTEGER NOT NULL,
  reasoning_tokens INTEGER NOT NULL DEFAULT 0,
  recorded_cost_usd REAL,
  calculated_cost_usd REAL NOT NULL,
  pricing_found INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS usage_session_pricing (
  provider TEXT NOT NULL,
  provider_session_id TEXT NOT NULL,
  fast INTEGER NOT NULL,
  evidence INTEGER NOT NULL,
  PRIMARY KEY (provider, provider_session_id)
);
CREATE INDEX IF NOT EXISTS usage_entries_time ON usage_entries(timestamp_ms);
CREATE INDEX IF NOT EXISTS usage_entries_session ON usage_entries(provider, provider_session_id);
CREATE INDEX IF NOT EXISTS usage_entries_source ON usage_entries(source_path);
`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize usage ledger: %w", err)
	}
	if err = ensureColumn(ctx, db, "usage_entries", "reasoning_tokens", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate usage ledger: %w", err)
	}
	if err := os.Chmod(s.options.Path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("protect usage ledger: %w", err)
	}
	return db, nil
}

// ensureColumn adds a column only when the table is known not to have it. A
// failed read of the table shape is reported, never treated as "the column is
// missing": guessing there would run an ALTER TABLE that fails with "duplicate
// column name" and take the whole ledger down with it.
func ensureColumn(ctx context.Context, db *sql.DB, table, column, definition string) error {
	found, err := columnExists(ctx, db, table, column)
	if err != nil || found {
		return err
	}
	return addColumn(ctx, db, table, column, definition)
}

func columnExists(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, err
	}
	found := false
	for rows.Next() {
		var cid int
		var name, kind string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return false, err
		}
		if name == column {
			found = true
		}
	}
	// An interrupted read reports zero columns. Believing it would mean adding
	// a column that is already there, so the error must travel.
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, fmt.Errorf("read %s columns: %w", table, err)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	return found, nil
}

func addColumn(ctx context.Context, db *sql.DB, table, column, definition string) error {
	if _, err := db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+column+" "+definition); err != nil {
		// Another writer may have added the column between the read and here.
		// That is the outcome we wanted, not a reason to fail the open.
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

func (s *Service) Close() error {
	s.initMu.Lock()
	defer s.initMu.Unlock()
	s.closed = true
	db := s.db
	s.db = nil
	if db == nil {
		return nil
	}
	return db.Close()
}
