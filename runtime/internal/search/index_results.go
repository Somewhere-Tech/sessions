package search

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
	_ "modernc.org/sqlite"
)

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
