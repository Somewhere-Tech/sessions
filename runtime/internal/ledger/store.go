package ledger

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/somewhere-tech/sessions/runtime/internal/providerargs"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
	_ "modernc.org/sqlite"
)

const (
	defaultBusyTimeout      = 5 * time.Second
	defaultActivityCoalesce = time.Second
)

const schema = `
CREATE TABLE IF NOT EXISTS lane_events (
    seq            INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id       TEXT NOT NULL UNIQUE,
    lane_id        TEXT NOT NULL,
    type           TEXT NOT NULL,
    at_ms          INTEGER NOT NULL,
    actor          TEXT NOT NULL,
    schema_version INTEGER NOT NULL,
    payload_json   TEXT NOT NULL CHECK (json_valid(payload_json))
);
CREATE INDEX IF NOT EXISTS lane_events_lane_seq ON lane_events(lane_id, seq);
CREATE INDEX IF NOT EXISTS lane_events_type_seq ON lane_events(type, seq);
CREATE TRIGGER IF NOT EXISTS lane_events_no_update
BEFORE UPDATE ON lane_events
BEGIN
    SELECT RAISE(ABORT, 'lane_events is append-only');
END;
CREATE TRIGGER IF NOT EXISTS lane_events_no_delete
BEFORE DELETE ON lane_events
BEGIN
    SELECT RAISE(ABORT, 'lane_events is append-only');
END;
`

type Store struct {
	db               *sql.DB
	path             string
	clock            func() time.Time
	newEventID       func() (string, error)
	activityWindowMS int64
}

type boundaryWriter struct{ store *Store }
type observationWriter struct{ store *Store }
type migrationWriter struct{ store *Store }
type retentionWriter struct{ store *Store }
type attributionWriter struct{ store *Store }

// DefaultPath resolves the ledger outside Sessions' runner state directory.
//
// The root is the platform user state root, not a hardcoded macOS layout: this
// is shared code reached on every default daemon boot, and the previous literal
// ~/Library/Application Support built a nonsense C:\Users\<user>\Library\...
// tree on Windows for the one file that must survive every crash.
func DefaultPath() (string, error) {
	root, err := state.UserStateRootFromEnv()
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, "ledger", "lanes.sqlite3")
	if _, statErr := os.Stat(path); statErr == nil {
		return path, nil
	}
	// An installation that already wrote the macOS-only Application Support
	// ledger keeps using it. Starting an empty ledger beside it would silently
	// drop the durable record of every lane that machine has ever run.
	if legacy, ok := legacyDarwinLedgerPath(); ok {
		if _, statErr := os.Stat(legacy); statErr == nil {
			return legacy, nil
		}
	}
	return path, nil
}

// legacyDarwinLedgerPath is the pre-parity macOS location. It is consulted for
// adoption only and is never created.
func legacyDarwinLedgerPath() (string, bool) {
	if goruntime.GOOS != "darwin" {
		return "", false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(home, "Library", "Application Support", "sessions", "ledger", "lanes.sqlite3"), true
}

// ResolvePath applies SESSIONS_LEDGER_PATH unless Options.Path is explicit.
func ResolvePath(explicit string) (string, error) {
	path := explicit
	if path == "" {
		path = os.Getenv("SESSIONS_LEDGER_PATH")
	}
	if path == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return "", err
		}
	}
	resolved, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve ledger path: %w", err)
	}
	return resolved, nil
}

func Open(ctx context.Context, options Options) (*Store, error) {
	path, err := ResolvePath(options.Path)
	if err != nil {
		return nil, err
	}
	if err := ensurePrivateDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create ledger: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("chmod ledger: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close ledger bootstrap file: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	// Pragmas are connection-local. One long-lived connection keeps FULL,
	// busy_timeout, and foreign_keys stable while WAL still coordinates with
	// other daemon/helper processes opening the same file.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	busy := options.BusyTimeout
	if busy <= 0 {
		busy = defaultBusyTimeout
	}
	activity := options.ActivityCoalesce
	if activity <= 0 {
		activity = defaultActivityCoalesce
	}
	clock := options.Clock
	if clock == nil {
		clock = time.Now
	}
	newEventID := options.NewEventID
	if newEventID == nil {
		newEventID = randomUUID
	}
	store := &Store{
		db: db, path: path, clock: clock, newEventID: newEventID,
		activityWindowMS: max(activity.Milliseconds(), 1),
	}
	if err := store.configure(ctx, busy); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.secureFiles(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func ensurePrivateDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create ledger directory: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat ledger directory: %w", err)
	}
	// An override may deliberately place the 0600 database directly in a
	// shared sticky scratch root such as /tmp. Never chmod such a root out
	// from under the rest of the machine; newly-created and dedicated ledger
	// directories still get the required owner-only mode below.
	home, _ := os.UserHomeDir()
	if filepath.Dir(dir) == dir || filepath.Clean(dir) == filepath.Clean(home) ||
		(info.Mode()&os.ModeSticky != 0 && info.Mode().Perm()&0o002 != 0) {
		return nil
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod ledger directory: %w", err)
	}
	return nil
}

func (s *Store) configure(ctx context.Context, busy time.Duration) error {
	var mode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return fmt.Errorf("enable ledger WAL: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("enable ledger WAL: sqlite selected %q", mode)
	}
	statements := []string{
		"PRAGMA synchronous=FULL",
		fmt.Sprintf("PRAGMA busy_timeout=%d", max(busy.Milliseconds(), 1)),
		"PRAGMA foreign_keys=ON",
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure ledger (%s): %w", statement, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("initialize ledger schema: %w", err)
	}
	return nil
}

func (s *Store) secureFiles() error {
	for _, path := range []string{s.path, s.path + "-wal", s.path + "-shm"} {
		if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("chmod ledger file %s: %w", path, err)
		}
	}
	return nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Boundaries() BoundaryWriter { return boundaryWriter{store: s} }

func (s *Store) Observations() ObservationWriter { return observationWriter{store: s} }

func (s *Store) Migrations() MigrationWriter { return migrationWriter{store: s} }

func (s *Store) Retention() RetentionWriter { return retentionWriter{store: s} }

func (s *Store) Attributions() AttributionWriter { return attributionWriter{store: s} }

func (w boundaryWriter) RecordCreated(ctx context.Context, value Created) error {
	if value.LaneID == "" {
		value.LaneID = value.LaneUUID
	}
	if value.LaneUUID == "" {
		value.LaneUUID = value.LaneID
	}
	if value.LaneUUID != value.LaneID {
		return fmt.Errorf("record created: lane UUID %q does not match lane id %q", value.LaneUUID, value.LaneID)
	}
	if err := validateResumeIdentity(value.Tool, value.ProviderUUID, value.ResumeArgv); err != nil {
		return fmt.Errorf("record created: %w", err)
	}
	if err := ValidateCreator(value.CreatorKind, value.CreatorID); err != nil {
		return fmt.Errorf("record created: %w", err)
	}
	if value.DelegationKind != "" && value.DelegationKind != "user" && value.DelegationKind != "agent" {
		return fmt.Errorf("record created: invalid delegation kind %q", value.DelegationKind)
	}
	if value.Actor == "" {
		value.Actor = ActorDaemon
	}
	value.Description = strings.TrimSpace(value.Description)
	if value.Description == "" {
		value.DescriptionSource = ""
	} else if value.DescriptionSource == "" {
		value.DescriptionSource = DescriptionExplicit
	} else if value.DescriptionSource != DescriptionExplicit {
		return fmt.Errorf("record created: invalid description source %q", value.DescriptionSource)
	}
	worktreeFields := 0
	for _, field := range []string{value.WorktreePath, value.Branch, value.Base, value.SourceRepo} {
		if strings.TrimSpace(field) != "" {
			worktreeFields++
		}
	}
	if worktreeFields != 0 && worktreeFields != 4 {
		return errors.New("record created: worktree provenance requires worktree path, branch, base, and source repo")
	}
	if value.WorktreePath != "" && filepath.Clean(value.WorktreePath) != filepath.Clean(value.Cwd) {
		return fmt.Errorf("record created: worktree path %q does not match cwd %q", value.WorktreePath, value.Cwd)
	}
	if (value.Profile == "") != (value.ConfigDir == "") {
		return errors.New("record created: profile provenance requires both profile and config dir")
	}
	if value.ConfigDir != "" && !filepath.IsAbs(value.ConfigDir) {
		return errors.New("record created: profile config dir must be absolute")
	}
	payload := createdPayload{
		Name: value.Name, Description: value.Description, DescriptionSource: value.DescriptionSource,
		Kind: value.Kind, Tool: value.Tool, Cwd: value.Cwd, Profile: value.Profile, ConfigDir: value.ConfigDir,
		WorktreePath: value.WorktreePath, Branch: value.Branch, Base: value.Base, SourceRepo: value.SourceRepo,
		ResumeArgv: append([]string{}, value.ResumeArgv...),
		LaneUUID:   value.LaneUUID, ProviderUUID: value.ProviderUUID,
		CreatorKind: value.CreatorKind, CreatorID: value.CreatorID, DelegationKind: value.DelegationKind,
	}
	return w.store.append(ctx, EventCreated, value.Meta, payload, false)
}

func (w boundaryWriter) RecordProviderRebound(ctx context.Context, value ProviderRebound) error {
	if value.ProviderUUID == "" || !providerargs.IsConversationUUID(value.ProviderUUID) {
		return fmt.Errorf("record provider rebound: invalid provider UUID %q", value.ProviderUUID)
	}
	if value.NewLaneID == "" {
		return errors.New("record provider rebound: new lane id is required")
	}
	if value.LaneID == value.NewLaneID {
		return errors.New("record provider rebound: old and new lane ids must differ")
	}
	if value.Actor == "" {
		value.Actor = ActorUser
	}
	payload := providerReboundPayload{ProviderUUID: value.ProviderUUID, NewLaneID: value.NewLaneID}
	return w.store.append(ctx, EventProviderRebound, value.Meta, payload, false)
}

func (w boundaryWriter) RecordUserKill(ctx context.Context, value UserKill) error {
	if (value.InitiatorKind == "") != (value.InitiatorID == "") {
		return errors.New("record user kill: initiator kind and id must both be set or both be empty")
	}
	if value.InitiatorKind != "" {
		if err := ValidateCreator(value.InitiatorKind, value.InitiatorID); err != nil {
			return fmt.Errorf("record user kill: %w", err)
		}
	}
	for _, field := range []struct {
		name  string
		value string
		limit int
	}{
		{name: "initiator name", value: value.InitiatorName, limit: 128},
		{name: "client", value: value.Client, limit: 64},
		{name: "reason", value: value.Reason, limit: 280},
		{name: "operation id", value: value.OperationID, limit: 128},
	} {
		if utf8.RuneCountInString(field.value) > field.limit ||
			strings.IndexFunc(field.value, unicode.IsControl) >= 0 {
			return fmt.Errorf("record user kill: invalid %s", field.name)
		}
	}
	if value.Actor == "" {
		value.Actor = ActorUser
	}
	payload := userKillPayload{
		InitiatorKind: value.InitiatorKind,
		InitiatorID:   value.InitiatorID,
		InitiatorName: value.InitiatorName,
		Client:        value.Client,
		Reason:        value.Reason,
		OperationID:   value.OperationID,
	}
	return w.store.append(ctx, EventUserKillRequested, value.Meta, payload, false)
}

func (w observationWriter) RecordLaunchStarted(ctx context.Context, value Observation) error {
	return w.store.observe(ctx, EventLaunchStarted, value.Meta, ActorDaemon, emptyPayload{})
}

func (w observationWriter) RecordRunnerReady(ctx context.Context, value Observation) error {
	return w.store.observe(ctx, EventRunnerReady, value.Meta, ActorRunner, emptyPayload{})
}

func (w observationWriter) RecordProviderBound(ctx context.Context, value ProviderBound) error {
	if err := validateResumeIdentity("", value.ProviderUUID, value.ResumeArgv); err != nil {
		return fmt.Errorf("record provider bound: %w", err)
	}
	payload := providerPayload{ProviderUUID: value.ProviderUUID, ResumeArgv: append([]string{}, value.ResumeArgv...)}
	return w.store.observe(ctx, EventProviderBound, value.Meta, ActorProvider, payload)
}

func (w observationWriter) RecordAttached(ctx context.Context, value Observation) error {
	return w.store.observe(ctx, EventAttached, value.Meta, ActorDaemon, emptyPayload{})
}

func (w observationWriter) RecordActivity(ctx context.Context, value Activity) error {
	if value.Source != ActivityHumanInput && value.Source != ActivitySessionInput && value.Source != ActivityProviderEvent {
		return fmt.Errorf("record activity: invalid source %q", value.Source)
	}
	actor := ActorUser
	if value.Source == ActivitySessionInput {
		actor = ActorDaemon
	} else if value.Source == ActivityProviderEvent {
		actor = ActorProvider
	}
	if value.Actor == "" {
		value.Actor = actor
	}
	return w.store.append(ctx, EventActivity, value.Meta, activityPayload{Source: value.Source}, true)
}

func (w attributionWriter) RecordMessageRelayed(ctx context.Context, value MessageRelayed) error {
	if value.Author.Kind != CreatorSession {
		return fmt.Errorf("record message relayed: author kind must be %q", CreatorSession)
	}
	if err := ValidateCreator(value.Author.Kind, value.Author.ID); err != nil {
		return fmt.Errorf("record message relayed: %w", err)
	}
	for _, field := range []struct {
		name  string
		value string
		limit int
	}{
		{name: "author name", value: value.Author.Name, limit: 128},
		{name: "client", value: value.Author.Client, limit: 64},
	} {
		if strings.TrimSpace(field.value) == "" ||
			utf8.RuneCountInString(field.value) > field.limit ||
			strings.IndexFunc(field.value, unicode.IsControl) >= 0 {
			return fmt.Errorf("record message relayed: invalid %s", field.name)
		}
	}
	for _, digest := range []struct {
		name  string
		value string
	}{
		{name: "content digest", value: value.ContentSHA256},
		{name: "normalized digest", value: value.NormalizedSHA256},
	} {
		decoded, err := hex.DecodeString(digest.value)
		if err != nil || len(decoded) != 32 {
			return fmt.Errorf("record message relayed: invalid %s", digest.name)
		}
	}
	if value.ContentBytes <= 0 || value.NormalizedBytes < 0 || value.NormalizedBytes > value.ContentBytes {
		return errors.New("record message relayed: invalid byte counts")
	}
	if value.Actor == "" {
		value.Actor = ActorDaemon
	}
	payload := messageRelayedPayload{
		Author:        value.Author,
		ContentSHA256: value.ContentSHA256, ContentBytes: value.ContentBytes,
		NormalizedSHA256: value.NormalizedSHA256, NormalizedBytes: value.NormalizedBytes,
	}
	return w.store.append(ctx, EventMessageRelayed, value.Meta, payload, false)
}

func (w observationWriter) RecordIdle(ctx context.Context, value Observation) error {
	return w.store.observe(ctx, EventIdle, value.Meta, ActorDaemon, emptyPayload{})
}

func (w observationWriter) RecordRenamed(ctx context.Context, value Rename) error {
	return w.store.observe(ctx, EventRenamed, value.Meta, ActorUser, renamePayload{Name: value.Name})
}

func (w observationWriter) RecordDescriptionDerived(ctx context.Context, value DescriptionDerived) error {
	value.Description = strings.TrimSpace(value.Description)
	if value.Description == "" {
		return errors.New("record derived description: description is required")
	}
	if value.Source != DescriptionFirstMessage {
		return fmt.Errorf("record derived description: invalid source %q", value.Source)
	}
	return w.store.observe(ctx, EventDescriptionDerived, value.Meta, ActorUser, descriptionPayload{
		Description: value.Description, Source: value.Source,
	})
}

func (w observationWriter) RecordRunnerExited(ctx context.Context, value RunnerExit) error {
	payload := runnerExitPayload{Code: value.Code, Signal: value.Signal}
	return w.store.observe(ctx, EventRunnerExited, value.Meta, ActorRunner, payload)
}

func (w observationWriter) RecordRunnerLost(ctx context.Context, value Observation) error {
	return w.store.observe(ctx, EventRunnerLost, value.Meta, ActorDaemon, emptyPayload{})
}

func (w observationWriter) RecordReaped(ctx context.Context, value Observation) error {
	return w.store.observe(ctx, EventReaped, value.Meta, ActorDaemon, emptyPayload{})
}

func (w observationWriter) RecordReopened(ctx context.Context, value Reopened) error {
	return w.store.observe(ctx, EventReopened, value.Meta, ActorRecovery, reopenedPayload{NewLaneID: value.NewLaneID})
}

func (w observationWriter) RecordDaemonRestart(ctx context.Context, value Observation) error {
	return w.store.observe(ctx, EventDaemonRestart, value.Meta, ActorDaemon, emptyPayload{})
}

func (w migrationWriter) RecordMovedTo(ctx context.Context, value MovedTo) error {
	if value.Actor == "" {
		value.Actor = ActorUser
	}
	if value.TargetEndpoint == "" || value.NewLaneID == "" {
		return errors.New("record moved_to: target endpoint and new lane id are required")
	}
	payload := movedToPayload{
		TargetEndpoint: value.TargetEndpoint, NewLaneID: value.NewLaneID, CheckpointRef: value.CheckpointRef,
	}
	return w.store.append(ctx, EventMovedTo, value.Meta, payload, false)
}

func (w migrationWriter) RecordMovedFrom(ctx context.Context, value MovedFrom) error {
	if value.Actor == "" {
		value.Actor = ActorDaemon
	}
	if value.SourceEndpoint == "" || value.SourceLaneID == "" {
		return errors.New("record moved_from: source endpoint and source lane id are required")
	}
	payload := movedFromPayload{SourceEndpoint: value.SourceEndpoint, SourceLaneID: value.SourceLaneID}
	return w.store.append(ctx, EventMovedFrom, value.Meta, payload, false)
}

func (w retentionWriter) RecordArchived(ctx context.Context, values []Archived) error {
	if len(values) == 0 {
		return nil
	}
	type row struct {
		eventID string
		meta    Meta
	}
	rows := make([]row, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value.LaneID == "" {
			return errors.New("record archived: lane id is required")
		}
		if _, duplicate := seen[value.LaneID]; duplicate {
			return fmt.Errorf("record archived: duplicate lane id %q", value.LaneID)
		}
		seen[value.LaneID] = struct{}{}
		if value.AtMS == 0 {
			value.AtMS = w.store.clock().UnixMilli()
		}
		if value.Actor == "" {
			value.Actor = ActorUser
		}
		if !validActor(value.Actor) {
			return fmt.Errorf("record archived: invalid actor %q", value.Actor)
		}
		eventID := value.EventID
		if eventID == "" {
			var err error
			eventID, err = w.store.newEventID()
			if err != nil {
				return fmt.Errorf("record archived: generate event id: %w", err)
			}
		}
		rows = append(rows, row{eventID: eventID, meta: value.Meta})
	}

	transaction, err := w.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record archived: begin: %w", err)
	}
	defer transaction.Rollback()
	for _, row := range rows {
		if _, err := transaction.ExecContext(ctx, `
INSERT INTO lane_events(event_id, lane_id, type, at_ms, actor, schema_version, payload_json)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
			row.eventID, row.meta.LaneID, string(EventArchived), row.meta.AtMS,
			string(row.meta.Actor), SchemaVersion, `{}`); err != nil {
			return fmt.Errorf("record archived %s: insert: %w", row.meta.LaneID, err)
		}
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("record archived: commit: %w", err)
	}
	if err := w.store.secureFiles(); err != nil {
		return fmt.Errorf("record archived: %w", err)
	}
	return nil
}

func (s *Store) observe(ctx context.Context, kind EventType, meta Meta, actor Actor, payload any) error {
	if meta.Actor == "" {
		meta.Actor = actor
	}
	return s.append(ctx, kind, meta, payload, false)
}

func (s *Store) append(ctx context.Context, kind EventType, meta Meta, payload any, coalesce bool) error {
	if meta.LaneID == "" {
		return fmt.Errorf("record %s: lane id is required", kind)
	}
	if meta.AtMS == 0 {
		meta.AtMS = s.clock().UnixMilli()
	}
	if meta.Actor == "" {
		return fmt.Errorf("record %s: actor is required", kind)
	}
	if !validActor(meta.Actor) {
		return fmt.Errorf("record %s: invalid actor %q", kind, meta.Actor)
	}
	eventID := meta.EventID
	if eventID == "" {
		var err error
		eventID, err = s.newEventID()
		if err != nil {
			return fmt.Errorf("record %s: generate event id: %w", kind, err)
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("record %s: encode payload: %w", kind, err)
	}
	if !json.Valid(encoded) {
		return fmt.Errorf("record %s: invalid JSON payload", kind)
	}

	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record %s: begin: %w", kind, err)
	}
	defer transaction.Rollback()
	if coalesce {
		var latest sql.NullInt64
		err = transaction.QueryRowContext(ctx,
			"SELECT MAX(at_ms) FROM lane_events WHERE lane_id = ? AND type = ? AND at_ms <= ?",
			meta.LaneID, string(kind), meta.AtMS,
		).Scan(&latest)
		if err != nil {
			return fmt.Errorf("record %s: read coalescing window: %w", kind, err)
		}
		if latest.Valid && meta.AtMS-latest.Int64 < s.activityWindowMS {
			return nil
		}
	}
	_, err = transaction.ExecContext(ctx, `
INSERT INTO lane_events(event_id, lane_id, type, at_ms, actor, schema_version, payload_json)
VALUES (?, ?, ?, ?, ?, ?, ?)`, eventID, meta.LaneID, string(kind), meta.AtMS, string(meta.Actor), SchemaVersion, string(encoded))
	if err != nil {
		return fmt.Errorf("record %s: insert: %w", kind, err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("record %s: commit: %w", kind, err)
	}
	if err := s.secureFiles(); err != nil {
		return fmt.Errorf("record %s: %w", kind, err)
	}
	return nil
}

func (s *Store) Events(ctx context.Context, laneID string) ([]Event, error) {
	query := `SELECT seq, event_id, lane_id, type, at_ms, actor, schema_version, payload_json FROM lane_events`
	args := make([]any, 0, 1)
	if laneID != "" {
		query += " WHERE lane_id = ?"
		args = append(args, laneID)
	}
	query += " ORDER BY seq"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read ledger events: %w", err)
	}
	defer rows.Close()
	events := make([]Event, 0)
	for rows.Next() {
		var event Event
		var kind, actor, payload string
		if err := rows.Scan(&event.Seq, &event.EventID, &event.LaneID, &kind, &event.AtMS, &actor, &event.SchemaVersion, &payload); err != nil {
			return nil, fmt.Errorf("scan ledger event: %w", err)
		}
		event.Type = EventType(kind)
		event.Actor = Actor(actor)
		event.Payload = json.RawMessage(payload)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read ledger events: %w", err)
	}
	return events, nil
}

func (s *Store) QuickCheck(ctx context.Context) error {
	var result string
	if err := s.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("ledger quick_check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("ledger quick_check: %s", result)
	}
	return nil
}

type emptyPayload struct{}

type createdPayload struct {
	Name              string            `json:"name,omitempty"`
	Description       string            `json:"description,omitempty"`
	DescriptionSource DescriptionSource `json:"description_source,omitempty"`
	Kind              string            `json:"kind,omitempty"`
	Tool              string            `json:"tool"`
	Cwd               string            `json:"cwd"`
	Profile           string            `json:"profile,omitempty"`
	ConfigDir         string            `json:"config_dir,omitempty"`
	WorktreePath      string            `json:"worktree_path,omitempty"`
	Branch            string            `json:"branch,omitempty"`
	Base              string            `json:"base,omitempty"`
	SourceRepo        string            `json:"source_repo,omitempty"`
	ResumeArgv        []string          `json:"argv"`
	LaneUUID          string            `json:"lane_uuid"`
	ProviderUUID      string            `json:"provider_uuid,omitempty"`
	CreatorKind       CreatorKind       `json:"creator_kind"`
	CreatorID         string            `json:"creator_id"`
	DelegationKind    string            `json:"delegation_kind,omitempty"`
}

type providerPayload struct {
	ProviderUUID string   `json:"provider_uuid"`
	ResumeArgv   []string `json:"argv"`
}

type providerReboundPayload struct {
	ProviderUUID string `json:"provider_uuid"`
	NewLaneID    string `json:"new_lane_id"`
}

type activityPayload struct {
	Source ActivitySource `json:"source"`
}

type messageRelayedPayload struct {
	Author           MessageAuthor `json:"author"`
	ContentSHA256    string        `json:"content_sha256"`
	ContentBytes     int           `json:"content_bytes"`
	NormalizedSHA256 string        `json:"normalized_sha256"`
	NormalizedBytes  int           `json:"normalized_bytes"`
}

// DecodeMessageRelayed validates and expands one durable attribution event.
func DecodeMessageRelayed(event Event) (MessageRelayed, error) {
	if event.Type != EventMessageRelayed {
		return MessageRelayed{}, fmt.Errorf("decode message relayed: event type is %q", event.Type)
	}
	var payload messageRelayedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return MessageRelayed{}, fmt.Errorf("decode message relayed: %w", err)
	}
	return MessageRelayed{
		Meta: Meta{
			EventID: event.EventID, LaneID: event.LaneID,
			AtMS: event.AtMS, Actor: event.Actor,
		},
		Author:        payload.Author,
		ContentSHA256: payload.ContentSHA256, ContentBytes: payload.ContentBytes,
		NormalizedSHA256: payload.NormalizedSHA256, NormalizedBytes: payload.NormalizedBytes,
	}, nil
}

type renamePayload struct {
	Name string `json:"name"`
}

type descriptionPayload struct {
	Description string            `json:"description"`
	Source      DescriptionSource `json:"description_source"`
}

type userKillPayload struct {
	InitiatorKind CreatorKind `json:"initiator_kind,omitempty"`
	InitiatorID   string      `json:"initiator_id,omitempty"`
	InitiatorName string      `json:"initiator_name,omitempty"`
	Client        string      `json:"client,omitempty"`
	Reason        string      `json:"reason,omitempty"`
	OperationID   string      `json:"operation_id,omitempty"`
}

type runnerExitPayload struct {
	Code   *int    `json:"code"`
	Signal *string `json:"signal"`
}

type reopenedPayload struct {
	NewLaneID string `json:"newLaneId"`
}

type movedToPayload struct {
	TargetEndpoint string `json:"target_endpoint"`
	NewLaneID      string `json:"new_lane_id"`
	CheckpointRef  string `json:"checkpoint_ref,omitempty"`
}

type movedFromPayload struct {
	SourceEndpoint string `json:"source_endpoint"`
	SourceLaneID   string `json:"source_lane_id"`
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

func validActor(actor Actor) bool {
	switch actor {
	case ActorUser, ActorDaemon, ActorRunner, ActorProvider, ActorRecovery, ActorAdopt:
		return true
	default:
		return false
	}
}

// ValidateCreator checks the shape of a provenance principal. Existence of a
// session creator is a higher-level ledger graph check performed by session.
func ValidateCreator(kind CreatorKind, id string) error {
	if strings.TrimSpace(id) != id || id == "" {
		return errors.New("creator id is required and must not contain surrounding whitespace")
	}
	switch kind {
	case CreatorSession:
		if !providerargs.IsConversationUUID(id) {
			return fmt.Errorf("invalid creator session UUID %q", id)
		}
	case CreatorUser:
		if !userCreatorPattern.MatchString(id) {
			return fmt.Errorf("invalid user creator id %q", id)
		}
	case CreatorExternal:
		if len(id) > 256 || strings.ContainsAny(id, "\r\n\x00") {
			return errors.New("external creator id must be at most 256 bytes without control separators")
		}
	default:
		return fmt.Errorf("invalid creator kind %q", kind)
	}
	return nil
}

func validateResumeIdentity(tool, providerUUID string, argv []string) error {
	if providerUUID == "" {
		if len(argv) != 0 {
			return errors.New("resume argv requires a provider UUID")
		}
		return nil
	}
	if !providerargs.IsConversationUUID(providerUUID) {
		return fmt.Errorf("invalid provider UUID %q", providerUUID)
	}
	if len(argv) == 0 {
		return errors.New("provider UUID requires a resume argv")
	}
	expected := ResumeRecipeForProvider(tool, argv[0], providerUUID)
	if tool == "" {
		base := strings.ToLower(filepath.Base(argv[0]))
		if base == "claude" {
			expected = ResumeRecipeForProvider("claude-code", argv[0], providerUUID)
		} else if base == "codex" {
			expected = ResumeRecipeForProvider("codex", argv[0], providerUUID)
		}
	}
	if !slices.Equal(argv, expected) {
		return errors.New("argv is not a minimal provider resume recipe")
	}
	return nil
}
