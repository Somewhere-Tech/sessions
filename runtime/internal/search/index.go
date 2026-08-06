package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	_ "modernc.org/sqlite"
)

// A session's own name and working directory are indexed, not stored beside the
// index. "Always find a session you lost" fails at the first hurdle if typing a
// session's exact title cannot return that session, and a title is the one
// string about a session a person reliably remembers. They are separate
// columns rather than appended to the body so bm25 can weight a title match
// above an incidental mention of the same words in a transcript.
const indexSchema = `
DROP TABLE IF EXISTS messages_v2;
DROP TABLE IF EXISTS session_fingerprint_v2;
DROP TABLE IF EXISTS messages_v3;
DROP TABLE IF EXISTS session_fingerprint_v3;
CREATE VIRTUAL TABLE IF NOT EXISTS messages_v4 USING fts5(
    session_id UNINDEXED, name, tool UNINDEXED, role UNINDEXED, kind UNINDEXED,
    ts UNINDEXED, ts_ms UNINDEXED, message_index UNINDEXED, message_id UNINDEXED,
    cwd, machine UNINDEXED, creator_kind UNINDEXED, creator_id UNINDEXED,
    text, tokenize='porter unicode61'
);
CREATE TABLE IF NOT EXISTS session_fingerprint_v4 (session_id TEXT PRIMARY KEY, fp TEXT NOT NULL);
`

const searchIndexVersion = "transcript-v6"

const (
	// rankedColumnWeights weights every column of messages_v4 in declaration
	// order. Only name, cwd, and text are indexed; the rest are listed so the
	// weights stay aligned with the columns if one is ever promoted. A title
	// match outranks a body mention, and a working-directory match sits between
	// the two: a path is a strong signal about which session someone means, but
	// weaker than the name they gave it.
	//
	// The name weight is deliberately moderate. A session's name repeats on
	// every one of its indexed rows, so weighting it heavily fills the page
	// with a session that merely happens to be named after a common word.
	// Measured on a 288-session corpus, raising it from 4 to 12 left rollup
	// recall identical and cost message-page recall, because the title pass in
	// rankedRollup — not the weight — is what guarantees a titled session
	// surfaces at all.
	rankedColumnWeights = "1.0, 4.0, 1.0, 1.0, 1.0, 1.0, 1.0, 1.0, 1.0, 2.0, 1.0, 1.0, 1.0, 1.0"
	// rankedTextColumn is the zero-based index of the text column, which
	// snippet() needs and which moves the moment the schema above changes.
	rankedTextColumn = 13
)

// rankedRelevance maps a bm25 score to an absolute [0,1) relevance. bm25 in
// FTS5 is a negative number whose magnitude grows with the quality of the
// match, and normalizing it across the returned page — as this package used to
// — makes the same match score differently at -n 5 and at -n 50 and pins the
// last row of every page at exactly zero. A caller cannot threshold on a number
// that depends on how many rows it asked for, so the mapping here depends on
// nothing but the match itself.
func rankedRelevance(score float64) float64 {
	magnitude := -score
	if magnitude <= 0 {
		return 0
	}
	return magnitude / (magnitude + rankedScoreSaturation)
}

const (
	rankedScoreSaturation = 6.0
	// rankedRecencyWeight is deliberately small. "The session where we..."
	// almost always means recently, but a caller searching for a distinctive
	// term from a year ago must still get it: recency reorders near-ties, it
	// does not outrank relevance.
	rankedRecencyWeight   = 0.25
	rankedRecencyHalfLife = 60.0
)

// rankedBlend blends absolute relevance with recency. The blend is a multiplier
// in [1-w, 1], so the result stays inside [0,1) and stays monotone in
// relevance: a more relevant match never scores below a less relevant one of
// the same age.
func rankedBlend(score float64, timestampMS, nowMS int64) float64 {
	relevance := rankedRelevance(score)
	if relevance == 0 {
		return 0
	}
	freshness := 0.0
	if timestampMS > 0 && nowMS > timestampMS {
		ageDays := float64(nowMS-timestampMS) / float64(24*time.Hour/time.Millisecond)
		freshness = math.Pow(0.5, ageDays/rankedRecencyHalfLife)
	} else if timestampMS > 0 {
		freshness = 1
	}
	return relevance * (1 - rankedRecencyWeight + rankedRecencyWeight*freshness)
}

// roundScore is the reported precision. Ranking uses the unrounded blend, so
// two matches that differ only past the fourth decimal keep their order instead
// of collapsing into a tie the sort would then break arbitrarily.
//
// It is only ever called on a row that matched, and a row that matched is never
// scored zero. bm25 returns approximately zero for a term that appears in every
// document, which is true and useless: a caller filtering on score > 0 would
// then discard results the index just told it were hits.
func roundScore(blended float64) float64 {
	return max(math.Round(blended*10000)/10000, rankedMinimumScore)
}

// rankedMinimumScore is the score of a match whose terms carry no selectivity.
const rankedMinimumScore = 0.0001

// scoredMatch keeps the unrounded rank beside the match it belongs to. Score on
// Match is the reported, rounded value; rank is what the ordering below uses.
type scoredMatch struct {
	match Match
	rank  float64
}

var rankedSearchGate sync.Mutex

type rankedSession struct {
	history integrations.HistorySession
	tool    string
}

func runRanked(ctx context.Context, source HistorySource, live []state.SessionInfo, options Options, indexPath string) (Response, error) {
	parsed, err := parseSearchQuery(options.Query, options.RawSyntax)
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
			err := db.QueryRowContext(ctx, "SELECT fp FROM session_fingerprint_v4 WHERE session_id = ?", candidate.history.ID).Scan(&existing)
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

	result, err := queryRanked(ctx, db, parsed, options)
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
	err = tx.QueryRowContext(ctx, "SELECT fp FROM session_fingerprint_v4 WHERE session_id = ?", session.history.ID).Scan(&existing)
	if err == nil && existing == fingerprint {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("finish unchanged search index refresh for %s: %w", session.history.ID, err)
		}
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read search index fingerprint for %s: %w", session.history.ID, err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM messages_v4 WHERE session_id = ?", session.history.ID); err != nil {
		return fmt.Errorf("clear search index session %s: %w", session.history.ID, err)
	}
	insert, err := tx.PrepareContext(ctx, `
INSERT INTO messages_v4(
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
INSERT INTO session_fingerprint_v4(session_id, fp) VALUES (?, ?)
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
	if _, err := tx.ExecContext(ctx, "DELETE FROM messages_v4 WHERE session_id = ?", sessionID); err != nil {
		return fmt.Errorf("clear stale search index session %s: %w", sessionID, err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM session_fingerprint_v4 WHERE session_id = ?", sessionID); err != nil {
		return fmt.Errorf("clear stale search index fingerprint %s: %w", sessionID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit stale search index cleanup for %s: %w", sessionID, err)
	}
	return nil
}

func purgeUnavailableSessions(ctx context.Context, db *sql.DB, available map[string]bool) error {
	rows, err := db.QueryContext(ctx, "SELECT session_id FROM session_fingerprint_v4")
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

// rankedFilters builds the WHERE clause every ranked query shares. The MATCH
// expression is always argument zero so a caller can swap one rung of the
// relaxation ladder for the next without rebuilding the filters.
func rankedFilters(options Options) (string, []any) {
	where := []string{
		"messages_v4 MATCH ?",
		"messages_v4.session_id IN (SELECT session_id FROM search_query_sessions)",
	}
	arguments := make([]any, 0, 4)
	if options.Role != "" {
		where = append(where, "messages_v4.role = ?")
		arguments = append(arguments, options.Role)
	}
	if options.Tool != "" {
		where = append(where, "messages_v4.tool = ?")
		arguments = append(arguments, options.Tool)
	}
	if options.SinceMS != 0 {
		where = append(where, "messages_v4.ts_ms >= ?")
		arguments = append(arguments, options.SinceMS)
	}
	if options.UntilMS != 0 {
		where = append(where, "messages_v4.ts_ms < ?")
		arguments = append(arguments, options.UntilMS)
	}
	return strings.Join(where, " AND "), arguments
}

func rankedMatchCount(ctx context.Context, db *sql.DB, expression, where string, filters []any) (int, error) {
	arguments := append([]any{expression}, filters...)
	var total int
	err := db.QueryRowContext(ctx, "SELECT count(*) FROM messages_v4 WHERE "+where, arguments...).Scan(&total)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		return 0, rankedQueryError(err)
	}
	return total, nil
}

// rankedPlan picks the tightest expression whose result set can be trusted.
//
// The old behaviour was a pure OR of every word including the stopwords, which
// on a real corpus matches most of it: an eight-word sentence returned 73% of
// every message ever indexed and let a row whose only merit was the word "the"
// rank second. Requiring every content term instead is a large precision gain
// that costs recall only when the conjunction is genuinely empty. Nothing here
// can return fewer results than the old OR: the last rung is that OR.
//
// It used to descend only when a rung matched nothing at all, and that is the
// part that was wrong. A rung which matches one message is not a precise
// answer, it is a coincidence, and stopping there hides the conversation the
// person is looking for behind an accident. Measured on the real corpus here,
// "where we talked about taking over the google ads account" matched exactly
// one message -- in an unrelated session -- because the target says "take over
// my Google Ads account" and never says "talked". Strict returned that one
// wrong session and the ladder stopped.
//
// Descending to the quorum rung does not fix it, which is why the tests pin the
// whole ladder rather than this one condition: quorum makes the RAREST terms
// mandatory, and a narration verb a person invents while describing a memory
// ("talked", 54 occurrences here) is rarer than the subject they are actually
// looking for ("google", 219). The word carrying none of the meaning becomes
// the one that cannot be dropped.
//
// What does work is ranking. Across seven sentence-length queries measured
// against this corpus, whose targets were established by hand first, the broad
// rung ranked the right session FIRST in all seven -- including both cases
// where a tighter rung was non-empty and wrong. So for a query long enough that
// requiring every word is a bet rather than a request, a tiny result set is
// evidence the rung is filtering out the answer, not evidence it found it.
//
// Short queries keep the old behaviour exactly. Asking for one identifier, or
// two words, is a request for precisely those words, and a single match is the
// answer rather than an accident.
func rankedPlan(ctx context.Context, db *sql.DB, parsed parsedQuery, where string, filters []any) (string, string, int, error) {
	if parsed.raw {
		total, err := rankedMatchCount(ctx, db, parsed.rawExpr, where, filters)
		if err != nil {
			return "", "", 0, err
		}
		return parsed.rawExpr, "raw", total, nil
	}
	strict := parsed.strictExpression()
	total, err := rankedMatchCount(ctx, db, strict, where, filters)
	if err != nil {
		return "", "", 0, err
	}
	if trustRung(total, parsed) {
		return strict, "strict", total, nil
	}
	// Document frequency is only worth a round trip once the conjunction has
	// already come back empty, so the common path never pays for it.
	order, err := rankedTermOrder(ctx, db, parsed, where, filters)
	if err != nil {
		return "", "", 0, err
	}
	if quorum := parsed.quorumExpression(order, max(2, (len(parsed.required)+1)/2)); quorum != "" {
		total, err := rankedMatchCount(ctx, db, quorum, where, filters)
		if err != nil {
			return "", "", 0, err
		}
		if trustRung(total, parsed) {
			return quorum, "quorum", total, nil
		}
	}
	broad := parsed.broadExpression()
	total, err = rankedMatchCount(ctx, db, broad, where, filters)
	if err != nil {
		return "", "", 0, err
	}
	return broad, "broad", total, nil
}

// proseQueryTerms is where a query stops reading as a request for particular
// words and starts reading as a remembered sentence. Below it every term is
// something the person deliberately chose; at and above it some are only
// connective tissue, and requiring all of them is a bet on phrasing the person
// has no reason to have reproduced.
const proseQueryTerms = 4

// trustRung reports whether a rung's result set is worth stopping on.
//
// For a short query anything at all is: those words were asked for. For a
// sentence the rung has to match at least as many messages as the person typed
// words -- a self-scaling bar, because the longer the sentence the less likely
// any single message legitimately contains every word of it, and the more
// likely a lone match is a coincidence that would hide the answer.
func trustRung(total int, parsed parsedQuery) bool {
	if len(parsed.required) < 2 {
		return true
	}
	if len(parsed.required) < proseQueryTerms {
		return total > 0
	}
	return total >= len(parsed.required)
}

// rankedTermOrder sorts the required terms rarest first, so relaxation keeps
// the terms that carry the query's meaning and lets the common ones go.
func rankedTermOrder(ctx context.Context, db *sql.DB, parsed parsedQuery, where string, filters []any) ([]int, error) {
	frequencies := make([]int, len(parsed.required))
	for index, term := range parsed.required {
		total, err := rankedMatchCount(ctx, db, term.expression, where, filters)
		if err != nil {
			return nil, err
		}
		frequencies[index] = total
	}
	order := make([]int, len(parsed.required))
	for index := range order {
		order[index] = index
	}
	sort.SliceStable(order, func(i, j int) bool {
		// A term nothing matches is not a useful anchor; it sorts last so
		// relaxation never anchors on a typo.
		left, right := frequencies[order[i]], frequencies[order[j]]
		if (left == 0) != (right == 0) {
			return right == 0
		}
		return left < right
	})
	return order, nil
}

func queryRanked(ctx context.Context, db *sql.DB, parsed parsedQuery, options Options) (Response, error) {
	where, filters := rankedFilters(options)
	expression, mode, totalHits, err := rankedPlan(ctx, db, parsed, where, filters)
	if err != nil {
		return Response{}, err
	}
	result := Response{
		Matches:        make([]Match, 0, min(options.Limit, 16)),
		EffectiveQuery: expression,
		MatchMode:      mode,
		TotalHits:      totalHits,
	}
	if totalHits == 0 {
		return result, nil
	}
	nowMS := time.Now().UnixMilli()
	if err := rankedRollup(ctx, db, parsed, expression, where, filters, nowMS, &result); err != nil {
		return Response{}, err
	}

	// Ordering is decided in Go, so the page has to be drawn from more rows
	// than it shows: recency blending and the per-session cap below both change
	// which rows belong on it.
	arguments := append(append([]any{expression}, filters...), rankedCandidateLimit(options.Limit))
	query := `
SELECT messages_v4.session_id, messages_v4.name, messages_v4.tool, messages_v4.role,
       messages_v4.kind, messages_v4.ts, messages_v4.ts_ms, messages_v4.message_index,
       messages_v4.message_id, messages_v4.text,
       snippet(messages_v4, ` + strconv.Itoa(rankedTextColumn) + `, '[[', ']]', '…', 32),
       bm25(messages_v4, ` + rankedColumnWeights + `),
       messages_v4.cwd, messages_v4.machine, messages_v4.creator_kind, messages_v4.creator_id
FROM messages_v4
WHERE ` + where + `
ORDER BY bm25(messages_v4, ` + rankedColumnWeights + `) ASC, messages_v4.rowid ASC
LIMIT ?`
	rows, err := db.QueryContext(ctx, query, arguments...)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Response{}, ctxErr
		}
		return Response{}, rankedQueryError(err)
	}
	defer rows.Close()

	candidates := make([]scoredMatch, 0, 64)
	highlight := rankedHighlightPatterns(parsed.highlights)
	for rows.Next() {
		var match Match
		var rawScore float64
		var timestampMS sql.NullInt64
		if err := rows.Scan(
			&match.SessionID, &match.Name, &match.Tool, &match.Role, &match.Kind,
			&match.Timestamp, &timestampMS, &match.MessageIndex, &match.MessageID, &match.Text,
			&match.Snippet, &rawScore, &match.CWD,
			&match.Machine, &match.CreatorKind, &match.CreatorID,
		); err != nil {
			return Response{}, fmt.Errorf("read ranked search result: %w", err)
		}
		match.MatchStart, match.MatchEnd = rankedHighlightSpan(highlight, match.Text)
		blended := rankedBlend(rawScore, timestampMS.Int64, nowMS)
		match.Score = roundScore(blended)
		candidates = append(candidates, scoredMatch{match: match, rank: blended})
	}
	if err := rows.Err(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Response{}, ctxErr
		}
		return Response{}, rankedQueryError(err)
	}
	if err := rows.Close(); err != nil {
		return Response{}, fmt.Errorf("close ranked search results: %w", err)
	}

	sortMatchesByScore(candidates)
	result.Matches = spreadMatchesAcrossSessions(candidates, options)
	if options.Context > 0 {
		for index := range result.Matches {
			before, after, err := rankedContext(ctx, db, result.Matches[index].SessionID, result.Matches[index].MessageIndex, options.Context)
			if err != nil {
				return Response{}, err
			}
			result.Matches[index].ContextBefore, result.Matches[index].ContextAfter = before, after
		}
	}
	rankedRollupSnippets(result.Matches, result.Sessions)
	if options.Timeline {
		sortMatchesTimeline(result.Matches)
	}
	result.Total = len(result.Matches)
	return result, nil
}

const (
	// rankedRollupScan bounds the rows the session rollup reads. A rollup over
	// a broad query would otherwise walk the whole corpus for a block nobody
	// reads past the first screen of.
	rankedRollupScan         = 6000
	rankedRollupSessions     = 50
	rankedRollupSnippetCount = 3
	rankedCandidateSpan      = 6
	rankedCandidateFloor     = 300
	rankedCandidateCap       = 3000
)

func rankedCandidateLimit(limit int) int {
	return min(max(limit*rankedCandidateSpan, rankedCandidateFloor), rankedCandidateCap)
}

// rankedRollup answers the question a caller actually asked. "Which session was
// that in" is not the same question as "which message", and a per-message page
// cannot answer it: one 2,303-message session can fill the page on its own and
// hide the twenty other sessions that also matched. The rollup is computed over
// every hit, not over the page, so its counts and its first/last timestamps
// describe the whole match set.
func rankedRollup(ctx context.Context, db *sql.DB, parsed parsedQuery, expression, where string, filters []any, nowMS int64, result *Response) error {
	order, byID, scanned, err := rankedRollupScanRows(ctx, db, expression, where, filters, nowMS, nil)
	if err != nil {
		return err
	}
	result.TotalSessions = len(order)
	// A truncated scan makes the per-session counts lower bounds, and saying so
	// is cheaper than letting a caller treat a capped number as exact.
	result.RollupPartial = scanned >= rankedRollupScan

	// The title pass runs against the name column alone. It is the difference
	// between "the session you named that" usually surfacing and always
	// surfacing: with 165 sessions mentioning "test" in their transcripts, the
	// four actually named "test" do not otherwise reach a capped rollup.
	if title := parsed.titleExpression(); title != "" {
		titleOrder, titleByID, _, err := rankedRollupScanRows(ctx, db, title, where, filters, nowMS, byID)
		if err != nil {
			return err
		}
		for _, sessionID := range titleOrder {
			entry, found := byID[sessionID]
			if !found {
				entry = titleByID[sessionID]
				byID[sessionID] = entry
				order = append(order, sessionID)
				result.TotalSessions++
			}
			entry.TitleMatch = true
		}
	}

	rollup := make([]SessionHits, 0, len(order))
	for _, sessionID := range order {
		rollup = append(rollup, *byID[sessionID])
	}
	sort.SliceStable(rollup, func(i, j int) bool {
		if rollup[i].TitleMatch != rollup[j].TitleMatch {
			return rollup[i].TitleMatch
		}
		if rollup[i].Score != rollup[j].Score {
			return rollup[i].Score > rollup[j].Score
		}
		return rollup[i].SessionID < rollup[j].SessionID
	})
	if len(rollup) > rankedRollupSessions {
		rollup = rollup[:rankedRollupSessions]
	}
	result.Sessions = rollup
	return nil
}

// rankedRollupScanRows folds a bounded, bm25-ordered scan into per-session
// totals. Aggregating in Go rather than in SQL is not a preference: FTS5
// refuses to evaluate bm25 inside an aggregate context, so the choice is this
// or a second ranking pass that would not agree with the first.
//
// Rows for sessions already present in `existing` are counted into that entry,
// so the title pass below can mark a session without double-counting its hits.
func rankedRollupScanRows(
	ctx context.Context, db *sql.DB, expression, where string, filters []any,
	nowMS int64, existing map[string]*SessionHits,
) ([]string, map[string]*SessionHits, int, error) {
	arguments := append(append([]any{expression}, filters...), rankedRollupScan)
	rows, err := db.QueryContext(ctx, `
SELECT messages_v4.session_id, messages_v4.name, messages_v4.cwd, messages_v4.tool,
       messages_v4.machine, messages_v4.ts, messages_v4.ts_ms,
       bm25(messages_v4, `+rankedColumnWeights+`)
FROM messages_v4
WHERE `+where+`
ORDER BY bm25(messages_v4, `+rankedColumnWeights+`) ASC, messages_v4.rowid ASC
LIMIT ?`, arguments...)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, 0, ctxErr
		}
		return nil, nil, 0, rankedQueryError(err)
	}
	defer rows.Close()

	order := make([]string, 0, 32)
	byID := make(map[string]*SessionHits, 32)
	scanned := 0
	for rows.Next() {
		var sessionID, name, cwd, tool, machine string
		var timestamp sql.NullString
		var timestampMS sql.NullInt64
		var rawScore float64
		if err := rows.Scan(&sessionID, &name, &cwd, &tool, &machine, &timestamp, &timestampMS, &rawScore); err != nil {
			return nil, nil, 0, fmt.Errorf("read ranked search rollup: %w", err)
		}
		scanned++
		if _, alreadyCounted := existing[sessionID]; alreadyCounted {
			if _, seen := byID[sessionID]; !seen {
				byID[sessionID] = existing[sessionID]
				order = append(order, sessionID)
			}
			continue
		}
		entry := byID[sessionID]
		if entry == nil {
			entry = &SessionHits{
				SessionID: sessionID, Name: name, CWD: cwd,
				Tool: tool, Machine: machine,
			}
			byID[sessionID] = entry
			order = append(order, sessionID)
		}
		entry.Hits++
		if score := roundScore(rankedBlend(rawScore, timestampMS.Int64, nowMS)); score > entry.Score {
			entry.Score = score
		}
		if timestamp.Valid && timestamp.String != "" {
			at := timestamp.String
			if entry.FirstHitAt == "" || at < entry.FirstHitAt {
				entry.FirstHitAt = at
			}
			if entry.LastHitAt == "" || at > entry.LastHitAt {
				entry.LastHitAt = at
			}
		}
	}
	if err := rows.Err(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, 0, ctxErr
		}
		return nil, nil, 0, rankedQueryError(err)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, 0, fmt.Errorf("close ranked search rollup: %w", err)
	}
	return order, byID, scanned, nil
}

func rankedRollupSnippets(matches []Match, sessions []SessionHits) {
	if len(sessions) == 0 {
		return
	}
	index := make(map[string]int, len(sessions))
	for position := range sessions {
		index[sessions[position].SessionID] = position
	}
	for _, match := range matches {
		position, found := index[match.SessionID]
		if !found || match.Snippet == "" {
			continue
		}
		if len(sessions[position].Snippets) >= rankedRollupSnippetCount {
			continue
		}
		sessions[position].Snippets = append(sessions[position].Snippets, match.Snippet)
	}
}

// sortMatchesByScore reorders the candidates the recency blend disagrees with.
// The slice arrives in bm25 order, and a stable sort keeps that order for
// everything the blend leaves tied.
func sortMatchesByScore(matches []scoredMatch) {
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].rank > matches[j].rank })
}

// spreadMatchesAcrossSessions fills the page in two passes so no single session
// can own it. Now that a session's name is indexed, every message of a session
// whose title matches is a match, and a 2,303-message session would otherwise
// return 2,303 identical-looking rows and bury every other session that
// answered the query. The second pass hands the remaining slots back in score
// order, so nothing is dropped that would otherwise have fit — only reordered.
func spreadMatchesAcrossSessions(candidates []scoredMatch, options Options) []Match {
	limit := options.Limit
	page := make([]Match, 0, min(limit, len(candidates)))
	// Searching inside one named session is a request for that session's
	// messages, so spreading would be sabotage.
	if len(candidates) <= limit || strings.TrimSpace(options.SessionID) != "" {
		for index, candidate := range candidates {
			if index == limit {
				break
			}
			page = append(page, candidate.match)
		}
		return page
	}
	perSession := max(3, limit/8)
	taken := make(map[string]int, 16)
	deferred := make([]Match, 0, len(candidates)-limit)
	for _, candidate := range candidates {
		if len(page) == limit {
			break
		}
		if taken[candidate.match.SessionID] >= perSession {
			deferred = append(deferred, candidate.match)
			continue
		}
		taken[candidate.match.SessionID]++
		page = append(page, candidate.match)
	}
	for _, match := range deferred {
		if len(page) == limit {
			break
		}
		page = append(page, match)
	}
	return page
}

var fts5SyntaxNearPattern = regexp.MustCompile(`syntax error near "([^"]*)"`)

// rankedQueryError turns a rejected query into instruction. FTS5 reports parse
// failures as raw SQLite text (`SQL logic error: fts5: syntax error near
// "NOT"`), which reads like a database fault and sends the reader debugging
// SQLite instead of the query they wrote. Anything that is not a parse failure
// is a real index fault the caller cannot fix by editing the query, so it stays
// a plain error and keeps its 500.
func rankedQueryError(err error) error {
	raw := err.Error()
	// SQLite reports a rejected MATCH expression as a logic error; a busy,
	// missing, or corrupt index reports its own class and is not the caller's
	// to repair.
	if !strings.Contains(raw, "SQL logic error") && !strings.Contains(raw, "fts5") {
		return fmt.Errorf("run ranked search: %w", err)
	}
	const guidance = "AND, OR, and NOT are raw-syntax operators and each needs a term on both sides, " +
		"quotes and parentheses must be balanced, and a quoted phrase is matched exactly. " +
		"Drop the fts: prefix to search these words as ordinary terms, " +
		`or search the text literally with --exact, for example: sessions search "NOT NULL" --exact`
	if strings.Contains(raw, "unterminated string") {
		return &optionError{message: "ranked search could not parse this query: a quote is opened and never closed. " + guidance}
	}
	if found := fts5SyntaxNearPattern.FindStringSubmatch(raw); found != nil {
		if found[1] == "" {
			return &optionError{message: "ranked search could not parse this query: it ends where a term was expected. " + guidance}
		}
		return &optionError{message: fmt.Sprintf(
			"ranked search could not parse this query near %q. %s", found[1], guidance,
		)}
	}
	return &optionError{message: "ranked search could not parse this query. " + guidance}
}

// rankedHighlightPatterns compiles the query terms a client can highlight, in
// query order. The terms come from the parsed query rather than from the raw
// string, so an excluded term and the raw-syntax marker never highlight.
// Compiling once per request keeps the per-row span cheap, and case folding
// belongs in the pattern rather than in a copy of the text: the offsets
// returned below have to index into the untouched message body.
func rankedHighlightPatterns(terms []string) []*regexp.Regexp {
	patterns := make([]*regexp.Regexp, 0, len(terms))
	for _, candidate := range terms {
		if candidate == "" {
			continue
		}
		pattern, err := regexp.Compile("(?i:" + regexp.QuoteMeta(candidate) + ")")
		if err != nil {
			continue
		}
		patterns = append(patterns, pattern)
	}
	return patterns
}

// rankedHighlightSpan locates the first query term inside a matching body.
// Searching a lowercased copy corrupts both ends of the span, because
// strings.ToLower is not length-preserving in Unicode (İ collapses to i, K to
// k): every offset after such a rune shifts, and the query term's own length is
// not the matched text's length. Matching the original bytes keeps the span
// usable against the body the anchored history route returns.
func rankedHighlightSpan(patterns []*regexp.Regexp, text string) (int, int) {
	for _, pattern := range patterns {
		if location := pattern.FindStringIndex(text); location != nil {
			return location[0], location[1]
		}
	}
	return 0, 0
}

func rankedContext(ctx context.Context, db *sql.DB, sessionID string, messageIndex, count int) ([]integrations.TranscriptMessage, []integrations.TranscriptMessage, error) {
	rows, err := db.QueryContext(ctx, `
SELECT role, kind, ts, message_index, message_id, text
FROM messages_v4
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
