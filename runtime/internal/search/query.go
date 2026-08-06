package search

import (
	"strings"
	"unicode"
)

// Ranked search is the query layer an agent actually reaches for, so the shape
// of a query it types has to survive the trip to FTS5 intact. Three shapes
// dominate: prose ("the session where we fixed the auth bug"), pasted paths
// ("runtime/internal/api/files.go"), and a session's own title. This file turns
// all three into FTS5 expressions without ever handing the raw text to the FTS5
// parser, which is what used to make an incidental "AND" in prose silently
// rewrite the whole query.

// RawSyntaxPrefix opts a single query into raw FTS5 syntax. The prefix is the
// only in-band way to reach the FTS5 parser, so an "AND" that a person merely
// typed inside a sentence can never be mistaken for the operator.
const RawSyntaxPrefix = "fts:"

// searchStopwords are the words that carry no selectivity in a conversation
// corpus. They are removed from the matching terms rather than merely
// down-weighted: a pure OR over them matches most of the corpus, and bm25
// cannot rescue a result set that already contains three quarters of every
// message ever indexed. The bare boolean words live here too, so prose that
// happens to contain "and" or "or" reads as prose.
var searchStopwords = map[string]bool{
	"a": true, "about": true, "all": true, "am": true, "an": true, "and": true,
	"any": true, "are": true, "as": true, "at": true, "be": true, "been": true,
	"but": true, "by": true, "can": true, "did": true, "do": true, "does": true,
	"for": true, "from": true, "had": true, "has": true, "have": true, "he": true,
	"her": true, "here": true, "his": true, "how": true, "i": true, "if": true,
	"in": true, "into": true, "is": true, "it": true, "its": true, "me": true,
	"my": true, "near": true, "no": true, "not": true, "of": true, "on": true,
	"one": true, "or": true, "our": true, "out": true, "over": true, "she": true,
	"so": true, "some": true, "than": true, "that": true, "the": true,
	"their": true, "them": true, "then": true, "there": true, "these": true,
	"they": true, "this": true, "those": true, "to": true, "up": true,
	"was": true, "we": true, "were": true, "what": true, "when": true,
	"where": true, "which": true, "while": true, "who": true, "why": true,
	"will": true, "with": true, "would": true, "you": true, "your": true,
}

type termKind int

const (
	termPlain termKind = iota
	termPhrase
	termPrefix
	termPath
)

type queryTerm struct {
	// text is what the caller typed, minus the marker that classified it. It is
	// the string a client highlights, so it keeps its original case.
	text string
	kind termKind
	// expression is the FTS5 clause this term contributes. Building it once at
	// parse time keeps every ladder rung below a join rather than a re-render.
	expression string
}

// parsedQuery is a bare-words query after classification. The three buckets are
// kept apart because each rung of the relaxation ladder combines them
// differently, and because the effective expression reported back to the caller
// has to be reconstructible without re-parsing.
type parsedQuery struct {
	raw        bool
	rawExpr    string
	required   []queryTerm
	dropped    []queryTerm
	highlights []string
}

type rawToken struct {
	text   string
	quoted bool
}

// splitQueryTokens splits on whitespace but keeps a double-quoted run together.
// An unterminated quote runs to the end of the input instead of failing: a
// pasted snippet with one stray quote is a query someone means, and refusing it
// costs the recall this package exists to provide.
func splitQueryTokens(query string) []rawToken {
	tokens := make([]rawToken, 0, 8)
	runes := []rune(query)
	for index := 0; index < len(runes); {
		if unicode.IsSpace(runes[index]) {
			index++
			continue
		}
		if runes[index] == '"' {
			index++
			start := index
			for index < len(runes) && runes[index] != '"' {
				index++
			}
			tokens = append(tokens, rawToken{text: string(runes[start:index]), quoted: true})
			if index < len(runes) {
				index++
			}
			continue
		}
		start := index
		for index < len(runes) && !unicode.IsSpace(runes[index]) && runes[index] != '"' {
			index++
		}
		tokens = append(tokens, rawToken{text: string(runes[start:index])})
	}
	return tokens
}

func hasTokenRunes(value string) bool {
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

func phraseLiteral(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func looksLikePath(value string) bool {
	if !strings.ContainsAny(value, `/\`) {
		return false
	}
	return len(pathComponents(value)) >= 2
}

func pathComponents(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == '/' || r == '\\' })
	components := make([]string, 0, len(parts))
	for _, part := range parts {
		if hasTokenRunes(part) {
			components = append(components, part)
		}
	}
	return components
}

// pathExpression makes a pasted path findable from either end. A path is one
// whitespace-delimited token, so quoting it whole asks FTS5 for those tokens
// contiguous — which is exactly wrong for the way people paste paths: an
// absolute path from one machine ("/Users/someone/files.go") shares only its
// tail with the repo-relative path the transcript actually contains. Emitting
// the full path plus its trailing suffixes matches on the tail while bm25 still
// rewards the rows that matched more of the path.
func pathExpression(value string) string {
	components := pathComponents(value)
	if len(components) < 2 {
		return phraseLiteral(value)
	}
	variants := make([]string, 0, 4)
	seen := make(map[string]bool, 4)
	add := func(candidate string) {
		if candidate == "" || seen[candidate] {
			return
		}
		seen[candidate] = true
		variants = append(variants, phraseLiteral(candidate))
	}
	add(value)
	for width := 3; width >= 1; width-- {
		if width >= len(components) {
			continue
		}
		add(strings.Join(components[len(components)-width:], "/"))
	}
	if len(variants) == 1 {
		return variants[0]
	}
	return "(" + strings.Join(variants, " OR ") + ")"
}

// parseSearchQuery classifies a query into the terms a ranked search runs.
// Nothing here can fail on user text: every rejection path this package still
// has belongs to raw syntax, which is opt-in.
func parseSearchQuery(query string, rawSyntax bool) (parsedQuery, error) {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return parsedQuery{}, &optionError{message: "q is required"}
	}
	if remainder, found := strings.CutPrefix(trimmed, RawSyntaxPrefix); found {
		rawSyntax = true
		trimmed = strings.TrimSpace(remainder)
	}
	// A near(a, b, n) call is a function shape nobody types by accident, so it
	// stays a proximity operator rather than becoming three literal terms.
	if nearExpressionPattern.MatchString(trimmed) {
		rawSyntax = true
	}
	if rawSyntax {
		expression := translateNearExpressions(trimmed)
		if strings.TrimSpace(expression) == "" {
			return parsedQuery{}, &optionError{message: "q is required"}
		}
		return parsedQuery{raw: true, rawExpr: expression, highlights: rawHighlights(trimmed)}, nil
	}

	parsed := parsedQuery{}
	for _, token := range splitQueryTokens(trimmed) {
		if token.quoted {
			if !hasTokenRunes(token.text) {
				continue
			}
			parsed.required = append(parsed.required, queryTerm{
				text: token.text, kind: termPhrase, expression: phraseLiteral(token.text),
			})
			parsed.highlights = append(parsed.highlights, token.text)
			continue
		}
		// A leading dash is not exclusion here. People paste "-race", "-n", and
		// "-SPEC.md" into searches far more often than they mean to exclude a
		// word, and a mistaken exclusion removes results silently — the exact
		// failure this package exists to stop. Exclusion lives in raw syntax,
		// where NOT is unambiguous and the effective query says so.
		text := token.text
		kind := termPlain
		if trimmedStar := strings.TrimRight(text, "*"); trimmedStar != text && trimmedStar != "" {
			kind = termPrefix
			text = trimmedStar
		}
		if !hasTokenRunes(text) {
			continue
		}
		var expression string
		switch {
		case kind == termPrefix:
			// The prefix marker has to sit outside the quotes; inside them FTS5
			// reads it as punctuation and silently narrows to the exact word.
			expression = phraseLiteral(text) + "*"
		case looksLikePath(text):
			kind = termPath
			expression = pathExpression(text)
		default:
			expression = phraseLiteral(text)
		}
		term := queryTerm{text: text, kind: kind, expression: expression}
		if kind == termPlain && searchStopwords[strings.ToLower(text)] {
			parsed.dropped = append(parsed.dropped, term)
			continue
		}
		parsed.required = append(parsed.required, term)
		parsed.highlights = append(parsed.highlights, text)
	}
	// A query made only of stopwords is still a query. Someone searching "the"
	// wants the rows containing "the", not an error.
	if len(parsed.required) == 0 && len(parsed.dropped) > 0 {
		parsed.required = parsed.dropped
		parsed.dropped = nil
		for _, term := range parsed.required {
			parsed.highlights = append(parsed.highlights, term.text)
		}
	}
	if len(parsed.required) == 0 {
		return parsedQuery{}, &optionError{
			message: "q has no searchable terms: ranked search matches words and numbers, " +
				"and this query has none. Search the text literally with --exact instead.",
		}
	}
	return parsed, nil
}

func rawHighlights(query string) []string {
	highlights := make([]string, 0, 4)
	for _, field := range strings.Fields(query) {
		candidate := strings.Trim(field, `"'(),*`)
		upper := strings.ToUpper(candidate)
		if candidate == "" || upper == "AND" || upper == "OR" || upper == "NOT" || strings.HasPrefix(upper, "NEAR") {
			continue
		}
		highlights = append(highlights, candidate)
	}
	return highlights
}

func termExpressions(terms []queryTerm) []string {
	expressions := make([]string, 0, len(terms))
	for _, term := range terms {
		expressions = append(expressions, term.expression)
	}
	return expressions
}

// strictExpression requires every content term. This is the rung that should
// answer almost every real query: it is the one that distinguishes the session
// someone is looking for from every session that merely says "the".
func (p parsedQuery) strictExpression() string {
	return strings.Join(termExpressions(p.required), " AND ")
}

// quorumExpression requires only the rarest of the terms and keeps the rest as
// an always-satisfiable OR group. FTS5 scores every phrase named in an
// expression, so the trailing group costs nothing in recall and still lifts the
// rows that happened to match the optional terms too.
//
// It takes at least three terms and at least two anchors for this rung to mean
// anything: anchoring a two-word query on one word returns a strict subset of
// what the broad rung below would return, ranked the same way, so it would be
// recall thrown away for nothing.
func (p parsedQuery) quorumExpression(order []int, keep int) string {
	if len(p.required) < 3 || keep < 2 || keep >= len(p.required) {
		return ""
	}
	anchors := make([]string, 0, keep)
	for _, index := range order[:keep] {
		anchors = append(anchors, p.required[index].expression)
	}
	boost := "(" + strings.Join(termExpressions(p.required), " OR ") + ")"
	return strings.Join(anchors, " AND ") + " AND " + boost
}

// broadExpression is the last rung. It is the behaviour ranked search used to
// have for every query; here it only runs once the conjunctive rungs have
// proven there is nothing tighter to return.
func (p parsedQuery) broadExpression() string {
	return strings.Join(termExpressions(p.required), " OR ")
}

// titleExpression restricts the query to the session-name column. Weighting a
// name match highly is not enough on its own: a common word appears in hundreds
// of transcripts and only a handful of titles, and the handful can be scrolled
// off the end of a rollup by sheer volume of body matches. Asking the name
// column directly is what makes "search a session's title, get that session" a
// guarantee rather than a tendency.
func (p parsedQuery) titleExpression() string {
	if p.raw || len(p.required) == 0 {
		return ""
	}
	return "name : (" + strings.Join(termExpressions(p.required), " AND ") + ")"
}
