package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	_ "modernc.org/sqlite"
)

const indexSchema = `
DROP TABLE IF EXISTS messages_v2;
DROP TABLE IF EXISTS session_fingerprint_v2;
CREATE VIRTUAL TABLE IF NOT EXISTS messages_v3 USING fts5(
    session_id UNINDEXED, name UNINDEXED, tool UNINDEXED, role UNINDEXED, kind UNINDEXED,
    ts UNINDEXED, ts_ms UNINDEXED, message_index UNINDEXED, message_id UNINDEXED,
    cwd UNINDEXED, machine UNINDEXED, creator_kind UNINDEXED, creator_id UNINDEXED,
    text, tokenize='porter unicode61'
);
CREATE TABLE IF NOT EXISTS session_fingerprint_v3 (session_id TEXT PRIMARY KEY, fp TEXT NOT NULL);
`

const searchIndexVersion = "transcript-v5"

var rankedSearchGate sync.Mutex

type rankedSession struct {
	history integrations.HistorySession
	tool    string
}

func runRanked(ctx context.Context, source HistorySource, live []state.SessionInfo, options Options, indexPath string) (Response, error) {
	matchExpression, err := rankedMatchExpression(options.Query)
	if err != nil {
		return Response{}, err
	}
	if strings.TrimSpace(indexPath) == "" {
		return Response{}, errors.New("search index path is required for ranked search")
	}
	rankedSearchGate.Lock()
	defer rankedSearchGate.Unlock()
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}

	sessions, err := source.SearchSessions(live)
	if err != nil {
		return Response{}, err
	}
	selectedIDs, err := resolveSessionIDs(sessions, options.SessionID)
	if err != nil {
		return Response{}, err
	}
	candidates := make([]rankedSession, 0, len(sessions))
	availableIDs := make(map[string]bool, len(sessions))
	for _, session := range sessions {
		if session.ConversationAvailable {
			availableIDs[session.ID] = true
		}
		tool := normalizeTool(session.Tool)
		if len(selectedIDs) > 0 && !selectedIDs[session.ID] {
			continue
		}
		if options.Tool != "" && tool != options.Tool {
			continue
		}
		if !sessionAllowed(session, options) {
			continue
		}
		if !session.ConversationAvailable {
			continue
		}
		candidates = append(candidates, rankedSession{history: session, tool: tool})
	}
	db, err := openIndex(ctx, indexPath)
	if err != nil {
		return Response{}, err
	}
	defer db.Close()
	if err := purgeUnavailableSessions(ctx, db, availableIDs); err != nil {
		return Response{}, err
	}
	if len(candidates) == 0 {
		return Response{Matches: []Match{}}, nil
	}

	querySessionIDs := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return Response{}, err
		}
		sourceFingerprint := rankedSourceFingerprint(candidate)
		if sourceFingerprint != "" {
			var existing string
			err := db.QueryRowContext(ctx, "SELECT fp FROM session_fingerprint_v3 WHERE session_id = ?", candidate.history.ID).Scan(&existing)
			if err == nil && existing == sourceFingerprint {
				querySessionIDs = append(querySessionIDs, candidate.history.ID)
				continue
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return Response{}, fmt.Errorf("read search index fingerprint for %s: %w", candidate.history.ID, err)
			}
		}
		transcript, err := source.TranscriptLimitedContext(ctx, live, candidate.history.ID, MaxFileReadBytes)
		if errors.Is(err, integrations.ErrHistoryNotFound) {
			if err := removeIndexedSession(ctx, db, candidate.history.ID); err != nil {
				return Response{}, err
			}
			continue
		}
		if err != nil {
			return Response{}, err
		}
		if err := refreshIndexedSession(ctx, db, candidate, transcript.Messages, sourceFingerprint); err != nil {
			return Response{}, err
		}
		querySessionIDs = append(querySessionIDs, candidate.history.ID)
	}
	if len(querySessionIDs) == 0 {
		return Response{Matches: []Match{}}, nil
	}
	if err := replaceQuerySessions(ctx, db, querySessionIDs); err != nil {
		return Response{}, err
	}

	result, err := queryRanked(ctx, db, matchExpression, options)
	if err != nil {
		return Response{}, err
	}
	providerSessions := make(map[string]string, len(candidates))
	for _, candidate := range candidates {
		providerSessions[candidate.history.ID] = candidate.history.ProviderSessionID
	}
	for index := range result.Matches {
		result.Matches[index].ProviderSessionID = providerSessions[result.Matches[index].SessionID]
	}
	return result, nil
}

func rankedMatchExpression(query string) (string, error) {
	if query == "" || strings.TrimSpace(query) == "" {
		return "", &optionError{message: "q is required"}
	}
	query = translateNearExpressions(strings.TrimSpace(query))
	fields := strings.Fields(query)
	explicitSyntax := strings.Contains(query, `"`)
	for _, field := range fields {
		if field == "AND" || field == "OR" || field == "NOT" {
			explicitSyntax = true
			break
		}
	}
	if explicitSyntax {
		return strings.TrimSpace(query), nil
	}
	quoted := make([]string, 0, len(fields))
	for _, field := range fields {
		quoted = append(quoted, `"`+strings.ReplaceAll(field, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " OR "), nil
}

var nearExpressionPattern = regexp.MustCompile(`(?i)near\(\s*([^,()]+?)\s*,\s*([^,()]+?)\s*,\s*([0-9]+)\s*\)`)

func translateNearExpressions(query string) string {
	return nearExpressionPattern.ReplaceAllStringFunc(query, func(value string) string {
		parts := nearExpressionPattern.FindStringSubmatch(value)
		if len(parts) != 4 {
			return value
		}
		left := strings.Trim(strings.TrimSpace(parts[1]), `"`)
		right := strings.Trim(strings.TrimSpace(parts[2]), `"`)
		return `NEAR("` + strings.ReplaceAll(left, `"`, `""`) + `" "` +
			strings.ReplaceAll(right, `"`, `""`) + `", ` + parts[3] + `)`
	})
}

func openIndex(ctx context.Context, path string) (*sql.DB, error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create search index directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create search index: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("chmod search index: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close search index bootstrap file: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open search index: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("configure search index: %w", err)
	}
	if _, err := db.ExecContext(ctx, indexSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize search index: %w", err)
	}
	return db, nil
}

func refreshIndexedSession(ctx context.Context, db *sql.DB, session rankedSession, messages []integrations.TranscriptMessage, fingerprint string) error {
	if fingerprint == "" {
		fingerprint = transcriptFingerprint(session, messages)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin search index refresh for %s: %w", session.history.ID, err)
	}
	defer tx.Rollback()

	var existing string
	err = tx.QueryRowContext(ctx, "SELECT fp FROM session_fingerprint_v3 WHERE session_id = ?", session.history.ID).Scan(&existing)
	if err == nil && existing == fingerprint {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("finish unchanged search index refresh for %s: %w", session.history.ID, err)
		}
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read search index fingerprint for %s: %w", session.history.ID, err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM messages_v3 WHERE session_id = ?", session.history.ID); err != nil {
		return fmt.Errorf("clear search index session %s: %w", session.history.ID, err)
	}
	insert, err := tx.PrepareContext(ctx, `
INSERT INTO messages_v3(
    session_id, name, tool, role, kind, ts, ts_ms, message_index, message_id,
    cwd, machine, creator_kind, creator_id, text
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare search index messages for %s: %w", session.history.ID, err)
	}
	defer insert.Close()
	for index, message := range messages {
		var timestamp any
		var timestampMS any
		if message.Timestamp != nil {
			timestamp = *message.Timestamp
			if parsed, ok := messageTimestampMS(message.Timestamp); ok {
				timestampMS = parsed
			}
		}
		if _, err := insert.ExecContext(
			ctx, session.history.ID, session.history.Name, session.tool, message.Role, message.Kind,
			timestamp, timestampMS, index, message.ID, session.history.CWD, session.history.Machine,
			session.history.CreatorKind, session.history.CreatorID, message.Text,
		); err != nil {
			return fmt.Errorf("index search message for %s: %w", session.history.ID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO session_fingerprint_v3(session_id, fp) VALUES (?, ?)
ON CONFLICT(session_id) DO UPDATE SET fp = excluded.fp`, session.history.ID, fingerprint); err != nil {
		return fmt.Errorf("write search index fingerprint for %s: %w", session.history.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit search index refresh for %s: %w", session.history.ID, err)
	}
	return nil
}

func transcriptFingerprint(session rankedSession, messages []integrations.TranscriptMessage) string {
	hash := fnv.New64a()
	writeFingerprintPart := func(value string) {
		_, _ = hash.Write([]byte(strconv.Itoa(len(value))))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0xff})
	}
	writeFingerprintPart(session.history.Name)
	writeFingerprintPart(session.tool)
	writeFingerprintPart(session.history.CWD)
	writeFingerprintPart(session.history.Machine)
	writeFingerprintPart(session.history.CreatorKind)
	writeFingerprintPart(session.history.CreatorID)
	writeFingerprintPart(searchIndexVersion)
	for _, message := range messages {
		writeFingerprintPart(message.Role)
		if message.Timestamp != nil {
			writeFingerprintPart(*message.Timestamp)
		} else {
			writeFingerprintPart("")
		}
		writeFingerprintPart(message.Text)
	}
	return fmt.Sprintf("%d:%016x", len(messages), hash.Sum64())
}

func rankedSourceFingerprint(session rankedSession) string {
	if session.history.SourceFingerprint == "" {
		return ""
	}
	return fmt.Sprintf(
		"%s:%s:%s:%s:%s:%s:%s",
		searchIndexVersion, session.history.ID, session.history.Name, session.tool,
		session.history.CWD, session.history.Machine, session.history.SourceFingerprint,
	)
}

func removeIndexedSession(ctx context.Context, db *sql.DB, sessionID string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin stale search index cleanup for %s: %w", sessionID, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM messages_v3 WHERE session_id = ?", sessionID); err != nil {
		return fmt.Errorf("clear stale search index session %s: %w", sessionID, err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM session_fingerprint_v3 WHERE session_id = ?", sessionID); err != nil {
		return fmt.Errorf("clear stale search index fingerprint %s: %w", sessionID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit stale search index cleanup for %s: %w", sessionID, err)
	}
	return nil
}

func purgeUnavailableSessions(ctx context.Context, db *sql.DB, available map[string]bool) error {
	rows, err := db.QueryContext(ctx, "SELECT session_id FROM session_fingerprint_v3")
	if err != nil {
		return fmt.Errorf("list indexed search sessions: %w", err)
	}
	stale := make([]string, 0)
	for rows.Next() {
		var sessionID string
		if err := rows.Scan(&sessionID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read indexed search session: %w", err)
		}
		if !available[sessionID] {
			stale = append(stale, sessionID)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("list indexed search sessions: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close indexed search sessions: %w", err)
	}
	for _, sessionID := range stale {
		if err := removeIndexedSession(ctx, db, sessionID); err != nil {
			return err
		}
	}
	return nil
}

func replaceQuerySessions(ctx context.Context, db *sql.DB, sessionIDs []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin ranked search session selection: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "CREATE TEMP TABLE IF NOT EXISTS search_query_sessions (session_id TEXT PRIMARY KEY)"); err != nil {
		return fmt.Errorf("create ranked search session selection: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM search_query_sessions"); err != nil {
		return fmt.Errorf("reset ranked search session selection: %w", err)
	}
	insert, err := tx.PrepareContext(ctx, "INSERT INTO search_query_sessions(session_id) VALUES (?)")
	if err != nil {
		return fmt.Errorf("prepare ranked search session selection: %w", err)
	}
	defer insert.Close()
	for _, sessionID := range sessionIDs {
		if _, err := insert.ExecContext(ctx, sessionID); err != nil {
			return fmt.Errorf("select ranked search session %s: %w", sessionID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit ranked search session selection: %w", err)
	}
	return nil
}

func queryRanked(ctx context.Context, db *sql.DB, matchExpression string, options Options) (Response, error) {
	where := []string{
		"messages_v3 MATCH ?",
		"messages_v3.session_id IN (SELECT session_id FROM search_query_sessions)",
	}
	arguments := []any{matchExpression}
	if options.Role != "" {
		where = append(where, "messages_v3.role = ?")
		arguments = append(arguments, options.Role)
	}
	if options.Tool != "" {
		where = append(where, "messages_v3.tool = ?")
		arguments = append(arguments, options.Tool)
	}
	if options.SinceMS != 0 {
		where = append(where, "messages_v3.ts_ms >= ?")
		arguments = append(arguments, options.SinceMS)
	}
	if options.UntilMS != 0 {
		where = append(where, "messages_v3.ts_ms < ?")
		arguments = append(arguments, options.UntilMS)
	}
	arguments = append(arguments, options.Limit)
	query := `
SELECT messages_v3.session_id, messages_v3.name, messages_v3.tool, messages_v3.role,
       messages_v3.kind, messages_v3.ts, messages_v3.message_index, messages_v3.message_id,
       messages_v3.text, snippet(messages_v3, 13, '[[', ']]', '…', 32), bm25(messages_v3),
       messages_v3.cwd, messages_v3.machine, messages_v3.creator_kind, messages_v3.creator_id
FROM messages_v3
WHERE ` + strings.Join(where, " AND ") + `
ORDER BY bm25(messages_v3) ASC, messages_v3.rowid ASC
LIMIT ?`
	rows, err := db.QueryContext(ctx, query, arguments...)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Response{}, ctxErr
		}
		return Response{}, &optionError{message: fmt.Sprintf("invalid ranked query: %v", err)}
	}
	defer rows.Close()

	result := Response{Matches: make([]Match, 0, min(options.Limit, 16))}
	rawScores := make([]float64, 0, min(options.Limit, 16))
	for rows.Next() {
		var match Match
		var rawScore float64
		if err := rows.Scan(
			&match.SessionID, &match.Name, &match.Tool, &match.Role, &match.Kind,
			&match.Timestamp, &match.MessageIndex, &match.MessageID, &match.Text,
			&match.Snippet, &rawScore, &match.CWD,
			&match.Machine, &match.CreatorKind, &match.CreatorID,
		); err != nil {
			return Response{}, fmt.Errorf("read ranked search result: %w", err)
		}
		match.MatchStart, match.MatchEnd = rankedHighlightSpan(match.Text, options.Query)
		result.Matches = append(result.Matches, match)
		rawScores = append(rawScores, rawScore)
	}
	if err := rows.Err(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Response{}, ctxErr
		}
		return Response{}, &optionError{message: fmt.Sprintf("invalid ranked query: %v", err)}
	}
	if err := rows.Close(); err != nil {
		return Response{}, fmt.Errorf("close ranked search results: %w", err)
	}
	if options.Context > 0 {
		for index := range result.Matches {
			before, after, err := rankedContext(ctx, db, result.Matches[index].SessionID, result.Matches[index].MessageIndex, options.Context)
			if err != nil {
				return Response{}, err
			}
			result.Matches[index].ContextBefore, result.Matches[index].ContextAfter = before, after
		}
	}
	normalizeRankedScores(result.Matches, rawScores)
	if options.Timeline {
		sortMatchesTimeline(result.Matches)
	}
	result.Total = len(result.Matches)
	return result, nil
}

func rankedHighlightSpan(text, query string) (int, int) {
	lower := strings.ToLower(text)
	for _, field := range strings.Fields(query) {
		candidate := strings.Trim(field, `"'(),`)
		upper := strings.ToUpper(candidate)
		if candidate == "" || upper == "AND" || upper == "OR" || upper == "NOT" || strings.HasPrefix(upper, "NEAR") {
			continue
		}
		if start := strings.Index(lower, strings.ToLower(candidate)); start >= 0 {
			return start, start + len(candidate)
		}
	}
	return 0, 0
}

func normalizeRankedScores(matches []Match, raw []float64) {
	if len(matches) == 0 {
		return
	}
	if len(matches) == 1 || raw[0] == raw[len(raw)-1] {
		for index := range matches {
			matches[index].Score = 1
		}
		return
	}
	best, worst := raw[0], raw[len(raw)-1]
	for index := range matches {
		matches[index].Score = 1 - ((raw[index] - best) / (worst - best))
	}
}

func rankedContext(ctx context.Context, db *sql.DB, sessionID string, messageIndex, count int) ([]integrations.TranscriptMessage, []integrations.TranscriptMessage, error) {
	rows, err := db.QueryContext(ctx, `
SELECT role, kind, ts, message_index, message_id, text
FROM messages_v3
WHERE session_id = ? AND message_index BETWEEN ? AND ?
ORDER BY message_index`, sessionID, messageIndex-count, messageIndex+count)
	if err != nil {
		return nil, nil, fmt.Errorf("read ranked search context: %w", err)
	}
	defer rows.Close()
	before := make([]integrations.TranscriptMessage, 0, count)
	after := make([]integrations.TranscriptMessage, 0, count)
	for rows.Next() {
		var message integrations.TranscriptMessage
		if err := rows.Scan(&message.Role, &message.Kind, &message.Timestamp, &message.Index, &message.ID, &message.Text); err != nil {
			return nil, nil, fmt.Errorf("decode ranked search context: %w", err)
		}
		if message.Index < messageIndex {
			before = append(before, message)
		} else if message.Index > messageIndex {
			after = append(after, message)
		}
	}
	return before, after, rows.Err()
}
