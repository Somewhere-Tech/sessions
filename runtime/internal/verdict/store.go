package verdict

import (
	"bufio"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
)

const maxRecordBytes = 2*1024*1024 + 64*1024

var safeIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var pathLocks sync.Map

type Options struct {
	StateDir   string
	LedgerPath string
	Clock      func() time.Time
}

type Store struct {
	stateDir   string
	ledgerPath string
	clock      func() time.Time
	mu         *sync.Mutex
}

func NewStore(options Options) (*Store, error) {
	if options.StateDir == "" {
		return nil, errors.New("verdict state directory is required")
	}
	stateDir, err := filepath.Abs(options.StateDir)
	if err != nil {
		return nil, fmt.Errorf("resolve verdict state directory: %w", err)
	}
	ledgerPath, err := ledger.ResolvePath(options.LedgerPath)
	if err != nil {
		return nil, err
	}
	clock := options.Clock
	if clock == nil {
		clock = time.Now
	}
	lock, _ := pathLocks.LoadOrStore(stateDir, &sync.Mutex{})
	return &Store{stateDir: stateDir, ledgerPath: ledgerPath, clock: clock, mu: lock.(*sync.Mutex)}, nil
}

func ValidateID(id string) error {
	if !safeIDPattern.MatchString(id) || id == "." || id == ".." {
		return fmt.Errorf("invalid verdict id %q", id)
	}
	return nil
}

func (s *Store) Path(id string) (string, error) {
	if err := ValidateID(id); err != nil {
		return "", err
	}
	return filepath.Join(s.stateDir, id+".verdicts.jsonl"), nil
}

func (s *Store) Emit(ctx context.Context, id string, document Document) (Record, error) {
	if err := Validate(document); err != nil {
		return Record{}, err
	}
	path, err := s.Path(id)
	if err != nil {
		return Record{}, err
	}
	if err := os.MkdirAll(s.stateDir, 0o700); err != nil {
		return Record{}, fmt.Errorf("create verdict state directory: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	latest, err := latestFromPath(path)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Record{}, err
	}
	now := s.clock().UTC()
	record := Record{
		SchemaVersion: document.SchemaVersion,
		Verdict:       document.Verdict,
		Findings:      append([]Finding(nil), document.Findings...),
		Meta:          cloneMeta(document.Meta),
		Seq:           latest.Seq + 1,
		EmittedAt:     now.Format(time.RFC3339Nano),
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return Record{}, fmt.Errorf("encode verdict: %w", err)
	}
	if len(encoded) > maxRecordBytes {
		return Record{}, fmt.Errorf("verdict record exceeds %d bytes", maxRecordBytes)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return Record{}, fmt.Errorf("open verdict log: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return Record{}, fmt.Errorf("secure verdict log: %w", err)
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		_ = file.Close()
		return Record{}, fmt.Errorf("append verdict log: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return Record{}, fmt.Errorf("sync verdict log: %w", err)
	}
	if err := file.Close(); err != nil {
		return Record{}, fmt.Errorf("close verdict log: %w", err)
	}
	if err := s.recordLedgerPointer(ctx, id, now); err != nil {
		return Record{}, err
	}
	return record, nil
}

func (s *Store) Latest(id string) (Record, error) {
	path, err := s.Path(id)
	if err != nil {
		return Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return latestFromPath(path)
}

func latestFromPath(path string) (Record, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("open verdict log: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), maxRecordBytes)
	var latest Record
	found := false
	line := 0
	for scanner.Scan() {
		line++
		trimmed := strings.TrimSpace(scanner.Text())
		if trimmed == "" {
			continue
		}
		var record Record
		if err := decodeStrict([]byte(trimmed), &record); err != nil {
			return Record{}, fmt.Errorf("decode verdict log line %d: %w", line, err)
		}
		if err := Validate(Document{
			SchemaVersion: record.SchemaVersion, Verdict: record.Verdict,
			Findings: record.Findings, Meta: record.Meta,
		}); err != nil {
			return Record{}, fmt.Errorf("decode verdict log line %d: %w", line, err)
		}
		if record.Seq == 0 || record.EmittedAt == "" {
			return Record{}, fmt.Errorf("decode verdict log line %d: missing sequence or emitted_at", line)
		}
		if _, err := time.Parse(time.RFC3339Nano, record.EmittedAt); err != nil {
			return Record{}, fmt.Errorf("decode verdict log line %d: invalid emitted_at: %w", line, err)
		}
		if (!found && record.Seq != 1) || (found && record.Seq != latest.Seq+1) {
			return Record{}, fmt.Errorf("decode verdict log line %d: sequence is not monotonic", line)
		}
		latest = record
		found = true
	}
	if err := scanner.Err(); err != nil {
		return Record{}, fmt.Errorf("read verdict log: %w", err)
	}
	if !found {
		return Record{}, ErrNotFound
	}
	return latest, nil
}

func cloneMeta(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	clone := make(map[string]any, len(value))
	for key, item := range value {
		clone[key] = item
	}
	return clone
}

// recordLedgerPointer appends only the existence/time pointer. The verdict,
// findings, metadata, and file path remain exclusively in the JSONL channel.
func (s *Store) recordLedgerPointer(ctx context.Context, laneID string, emittedAt time.Time) error {
	if _, err := os.Stat(s.ledgerPath); err != nil {
		return fmt.Errorf("stat ledger for verdict pointer: %w", err)
	}
	database, err := sql.Open("sqlite", s.ledgerPath)
	if err != nil {
		return fmt.Errorf("open ledger for verdict pointer: %w", err)
	}
	defer database.Close()
	if _, err := database.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		return fmt.Errorf("configure verdict ledger writer: %w", err)
	}
	eventID, err := randomUUID()
	if err != nil {
		return fmt.Errorf("generate verdict ledger event id: %w", err)
	}
	_, err = database.ExecContext(ctx, `
INSERT INTO lane_events(event_id, lane_id, type, at_ms, actor, schema_version, payload_json)
VALUES (?, ?, ?, ?, ?, ?, ?)`, eventID, laneID, "verdict", emittedAt.UnixMilli(), string(ledger.ActorProvider), ledger.SchemaVersion, "{}")
	if err != nil {
		return fmt.Errorf("record verdict ledger pointer: %w", err)
	}
	for _, path := range []string{s.ledgerPath, s.ledgerPath + "-wal", s.ledgerPath + "-shm"} {
		if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("secure verdict ledger file: %w", err)
		}
	}
	return nil
}

func randomUUID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}
