// Package search matches text in normalized Claude and Codex conversation
// history. Conversation discovery and parsing remain owned by integrations and
// watch; this package only filters normalized messages and builds snippets.
package search

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

const (
	DefaultLimit     = 100
	MaxLimit         = 1000
	MaxFileReadBytes = 0
	SnippetContext   = 96
	MaxSnippetRunes  = 240
	MaxContext       = 20
)

type Options struct {
	Query     string
	SessionID string
	Role      string
	Tool      string
	NameGlob  string
	CWD       string
	SinceMS   int64
	UntilMS   int64
	Context   int
	Timeline  bool
	Regex     bool
	Ranked    bool
	// RawSyntax hands the query to the FTS5 parser verbatim. It is opt-in
	// because the alternative — inferring it from a bare AND, OR, or NOT
	// anywhere in the text — silently rewrote ordinary prose into a boolean
	// expression and collapsed its recall with nothing in the response to say
	// so. A caller that cannot set this field can prefix the query with "fts:"
	// instead.
	RawSyntax bool
	Limit     int
}

type Match struct {
	SessionID         string  `json:"session_id"`
	ProviderSessionID string  `json:"provider_session_id,omitempty"`
	Name              string  `json:"name"`
	Tool              string  `json:"tool"`
	Role              string  `json:"role"`
	Kind              string  `json:"kind,omitempty"`
	Timestamp         *string `json:"timestamp"`
	MessageIndex      int     `json:"message_index"`
	MessageID         string  `json:"message_id"`
	Text              string  `json:"-"`
	Snippet           string  `json:"snippet"`
	// MatchStart and MatchEnd are byte offsets into the matching message body.
	// The body itself is deliberately not part of a search result; a client
	// that opens the anchored history route for SessionID/MessageIndex gets the
	// exact same text these offsets index into, so they must be computed
	// against that text and never against a transformed copy of it.
	MatchStart    int                              `json:"match_start"`
	MatchEnd      int                              `json:"match_end"`
	Score         float64                          `json:"score"`
	CWD           string                           `json:"cwd"`
	Machine       string                           `json:"machine"`
	CreatorKind   string                           `json:"creator_kind,omitempty"`
	CreatorID     string                           `json:"creator_id,omitempty"`
	ContextBefore []integrations.TranscriptMessage `json:"context_before,omitempty"`
	ContextAfter  []integrations.TranscriptMessage `json:"context_after,omitempty"`
	MachineAlias  string                           `json:"machine_alias,omitempty"`
	Reference     string                           `json:"reference,omitempty"`
	AvailableOn   []string                         `json:"available_on,omitempty"`
}

type Response struct {
	Matches  []Match        `json:"matches"`
	Total    int            `json:"total"`
	Machines []MachineState `json:"machines,omitempty"`
	Partial  bool           `json:"partial,omitempty"`
	// Sessions rolls the matches up to the question that was actually asked.
	// A caller looking for a lost session wants to know which session, and a
	// page of messages cannot say: the counts and timestamps here cover every
	// hit, not just the page.
	Sessions []SessionHits `json:"sessions,omitempty"`
	// EffectiveQuery is the expression that ran. A query layer that rewrites
	// what it was given — dropping stopwords, requiring terms, expanding a
	// pasted path, relaxing a conjunction that matched nothing — owes the
	// caller the result of that rewrite, or the caller cannot tell a real
	// absence from a query it should have phrased differently.
	EffectiveQuery string `json:"effective_query,omitempty"`
	// MatchMode names the rung of the relaxation ladder that produced these
	// results: strict, quorum, broad, or raw.
	MatchMode string `json:"match_mode,omitempty"`
	// TotalHits counts every matching message in the index. Total counts the
	// ones on this page, and the two differ whenever a page was truncated.
	TotalHits int `json:"total_hits,omitempty"`
	// TotalSessions counts the distinct sessions behind TotalHits.
	TotalSessions int `json:"total_sessions,omitempty"`
	// RollupPartial marks a rollup whose per-session counts are lower bounds
	// because the scan behind it was truncated.
	RollupPartial bool `json:"rollup_partial,omitempty"`
}

// SessionHits summarizes one session's contribution to a result set.
type SessionHits struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
	CWD       string `json:"cwd,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Machine   string `json:"machine,omitempty"`
	Hits      int    `json:"hits"`
	// TitleMatch marks a session whose own name satisfies the query. It is the
	// strongest available answer to "which session was that": someone who
	// remembers a title remembers it exactly.
	TitleMatch bool `json:"title_match,omitempty"`
	// Score is the best score any message in this session reached, on the same
	// absolute scale as Match.Score.
	Score float64 `json:"score"`
	// FirstHitAt and LastHitAt bracket every hit in this session, not just the
	// ones on the page, so they say how long the subject was live in it.
	FirstHitAt string `json:"first_hit_at,omitempty"`
	LastHitAt  string `json:"last_hit_at,omitempty"`
	// Snippets are drawn from the returned page, so a session that matched but
	// did not reach the page has counts and timestamps without them.
	Snippets []string `json:"snippets,omitempty"`
}

type MachineState struct {
	Alias    string `json:"alias"`
	Name     string `json:"name"`
	Endpoint string `json:"endpoint,omitempty"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
}

type HistorySource interface {
	SearchSessions([]state.SessionInfo) ([]integrations.HistorySession, error)
	TranscriptLimitedContext(context.Context, []state.SessionInfo, string, int64) (integrations.TranscriptResponse, error)
}

type optionError struct{ message string }

func (e *optionError) Error() string { return e.message }

func IsOptionError(err error) bool {
	var target *optionError
	return errors.As(err, &target)
}

type matcher struct {
	pattern *regexp.Regexp
}

func newMatcher(query string, regex bool) (matcher, error) {
	if query == "" {
		return matcher{}, &optionError{message: "q is required"}
	}
	if !regex {
		pattern, err := regexp.Compile("(?i:" + regexp.QuoteMeta(query) + ")")
		if err != nil {
			return matcher{}, &optionError{message: fmt.Sprintf("invalid query: %v", err)}
		}
		return matcher{pattern: pattern}, nil
	}
	pattern, err := regexp.Compile(query)
	if err != nil {
		return matcher{}, &optionError{message: fmt.Sprintf("invalid regex: %v", err)}
	}
	return matcher{pattern: pattern}, nil
}

func (m matcher) find(text string) (int, int, bool) {
	location := m.pattern.FindStringIndex(text)
	if location == nil {
		return 0, 0, false
	}
	return location[0], location[1], true
}

// Run searches known sessions in recent-activity order and messages in their
// normalized conversation order. One message contributes at most one match.
func Run(ctx context.Context, source HistorySource, live []state.SessionInfo, options Options, indexPath string) (Response, error) {
	options.Role = strings.ToLower(strings.TrimSpace(options.Role))
	options.Tool = strings.ToLower(strings.TrimSpace(options.Tool))
	options.NameGlob = strings.TrimSpace(options.NameGlob)
	options.CWD = strings.TrimSpace(options.CWD)
	if options.NameGlob != "" {
		if _, err := filepath.Match(options.NameGlob, "session"); err != nil {
			return Response{}, &optionError{message: fmt.Sprintf("invalid session name glob: %v", err)}
		}
	}
	if options.Ranked && options.Regex {
		return Response{}, &optionError{message: "--ranked cannot combine with --regex"}
	}
	if options.Role != "" && options.Role != "user" && options.Role != "assistant" && options.Role != "tool" {
		return Response{}, &optionError{message: "role must be user, assistant, or tool"}
	}
	if options.Tool != "" && options.Tool != "claude" && options.Tool != "codex" && options.Tool != "shell" {
		return Response{}, &optionError{message: "tool must be claude, codex, or shell"}
	}
	if options.Limit == 0 {
		options.Limit = DefaultLimit
	}
	if options.Limit < 1 || options.Limit > MaxLimit {
		return Response{}, &optionError{message: fmt.Sprintf("limit must be between 1 and %d", MaxLimit)}
	}
	if options.Context < 0 || options.Context > MaxContext {
		return Response{}, &optionError{message: fmt.Sprintf("context must be between 0 and %d", MaxContext)}
	}
	if options.SinceMS != 0 && options.UntilMS != 0 && options.SinceMS >= options.UntilMS {
		return Response{}, &optionError{message: "since must be before until"}
	}
	if options.Ranked {
		return runRanked(ctx, source, live, options, indexPath)
	}
	matchText, err := newMatcher(options.Query, options.Regex)
	if err != nil {
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
	result := Response{Matches: make([]Match, 0, min(options.Limit, 16))}
	// Collect first, order second. Timeline order is a property of the whole
	// result set, so reaching the limit has to leave the loops rather than
	// return from inside them and skip the sort below.
collect:
	for _, session := range sessions {
		if err := ctx.Err(); err != nil {
			return Response{}, err
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
		transcript, err := source.TranscriptLimitedContext(ctx, live, session.ID, MaxFileReadBytes)
		if errors.Is(err, integrations.ErrHistoryNotFound) {
			continue
		}
		if err != nil {
			return Response{}, err
		}
		for index, message := range transcript.Messages {
			if !messageAllowed(message, options) {
				continue
			}
			start, end, ok := matchText.find(message.Text)
			if !ok {
				continue
			}
			result.Matches = append(result.Matches, Match{
				SessionID: session.ID, ProviderSessionID: session.ProviderSessionID,
				Name: session.Name, Tool: tool,
				Role: message.Role, Timestamp: message.Timestamp, Text: message.Text,
				Kind: message.Kind, MessageID: message.ID,
				MessageIndex: index, Snippet: makeSnippet(message.Text, start, end),
				MatchStart: start, MatchEnd: end, Score: 1, CWD: session.CWD,
				Machine: session.Machine, CreatorKind: session.CreatorKind,
				CreatorID:     session.CreatorID,
				ContextBefore: contextBefore(transcript.Messages, index, options.Context),
				ContextAfter:  contextAfter(transcript.Messages, index, options.Context),
			})
			if len(result.Matches) == options.Limit {
				break collect
			}
		}
	}
	if options.Timeline {
		sortMatchesTimeline(result.Matches)
	}
	result.Total = len(result.Matches)
	return result, nil
}

func normalizeTool(tool string) string {
	switch tool {
	case "claude-code", "claude":
		return "claude"
	case "terminal", "":
		return "shell"
	default:
		return tool
	}
}

func resolveSessionIDs(sessions []integrations.HistorySession, requested string) (map[string]bool, error) {
	selected := make(map[string]bool)
	for _, value := range strings.Split(requested, ",") {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		resolved, err := resolveOneSessionID(sessions, value)
		if err != nil {
			return nil, err
		}
		selected[resolved] = true
	}
	return selected, nil
}

func resolveOneSessionID(sessions []integrations.HistorySession, requested string) (string, error) {
	for _, session := range sessions {
		if session.ID == requested {
			return session.ID, nil
		}
	}
	matches := make([]string, 0, 2)
	for _, session := range sessions {
		if strings.HasPrefix(session.ID, requested) {
			matches = append(matches, session.ID)
		}
	}
	switch len(matches) {
	case 0:
		return "", &optionError{message: fmt.Sprintf("no history session matching %q", requested)}
	case 1:
		return matches[0], nil
	default:
		return "", &optionError{message: fmt.Sprintf("ambiguous history session prefix %q", requested)}
	}
}

func sessionAllowed(session integrations.HistorySession, options Options) bool {
	if options.NameGlob != "" {
		matched, err := filepath.Match(strings.ToLower(options.NameGlob), strings.ToLower(session.Name))
		if err != nil || !matched {
			return false
		}
	}
	if options.CWD != "" {
		want := filepath.Clean(options.CWD)
		got := filepath.Clean(session.CWD)
		if got != want && !strings.HasPrefix(got, want+string(filepath.Separator)) {
			return false
		}
	}
	return true
}

func messageAllowed(message integrations.TranscriptMessage, options Options) bool {
	if options.Role != "" && message.Role != options.Role {
		return false
	}
	if options.SinceMS == 0 && options.UntilMS == 0 {
		return true
	}
	timestamp, ok := messageTimestampMS(message.Timestamp)
	if !ok {
		return false
	}
	return (options.SinceMS == 0 || timestamp >= options.SinceMS) &&
		(options.UntilMS == 0 || timestamp < options.UntilMS)
}

func messageTimestampMS(value *string) (int64, bool) {
	if value == nil || *value == "" {
		return 0, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, *value)
	if err != nil {
		return 0, false
	}
	return parsed.UnixMilli(), true
}

func contextBefore(messages []integrations.TranscriptMessage, index, count int) []integrations.TranscriptMessage {
	if count <= 0 || index <= 0 {
		return nil
	}
	start := max(0, index-count)
	return append([]integrations.TranscriptMessage(nil), messages[start:index]...)
}

func contextAfter(messages []integrations.TranscriptMessage, index, count int) []integrations.TranscriptMessage {
	if count <= 0 || index+1 >= len(messages) {
		return nil
	}
	end := min(len(messages), index+1+count)
	return append([]integrations.TranscriptMessage(nil), messages[index+1:end]...)
}

func sortMatchesTimeline(matches []Match) {
	sort.SliceStable(matches, func(i, j int) bool {
		left, leftOK := messageTimestampMS(matches[i].Timestamp)
		right, rightOK := messageTimestampMS(matches[j].Timestamp)
		if leftOK != rightOK {
			return leftOK
		}
		if left != right {
			return left < right
		}
		if matches[i].SessionID != matches[j].SessionID {
			return matches[i].SessionID < matches[j].SessionID
		}
		return matches[i].MessageIndex < matches[j].MessageIndex
	})
}

func makeSnippet(text string, matchStart, matchEnd int) string {
	if matchStart < 0 || matchEnd < matchStart || matchEnd > len(text) {
		return ""
	}
	startRune := utf8.RuneCountInString(text[:matchStart])
	endRune := startRune + utf8.RuneCountInString(text[matchStart:matchEnd])
	runes := []rune(text)
	matchRunes := endRune - startRune
	if matchRunes >= MaxSnippetRunes {
		highlight := runes[startRune : startRune+MaxSnippetRunes-1]
		return "[[" + collapseWhitespace(string(highlight)) + "…]]"
	}
	contextBudget := MaxSnippetRunes - matchRunes
	leftBudget := min(SnippetContext, contextBudget/2)
	rightBudget := min(SnippetContext, contextBudget-leftBudget)
	leftAvailable := startRune
	rightAvailable := len(runes) - endRune
	if leftAvailable < leftBudget {
		rightBudget = min(rightAvailable, rightBudget+(leftBudget-leftAvailable))
		leftBudget = leftAvailable
	}
	if rightAvailable < rightBudget {
		leftBudget = min(leftAvailable, leftBudget+(rightBudget-rightAvailable))
		rightBudget = rightAvailable
	}
	start := startRune - leftBudget
	end := endRune + rightBudget
	var output strings.Builder
	if start > 0 {
		output.WriteRune('…')
	}
	output.WriteString(string(runes[start:startRune]))
	output.WriteString("[[")
	output.WriteString(string(runes[startRune:endRune]))
	output.WriteString("]]")
	output.WriteString(string(runes[endRune:end]))
	if end < len(runes) {
		output.WriteRune('…')
	}
	return collapseWhitespace(output.String())
}

func collapseWhitespace(value string) string {
	return strings.Join(strings.FieldsFunc(value, unicode.IsSpace), " ")
}
