// Package session owns daemon-side session lifecycle and activity semantics.
package session

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/somewhere-tech/sessions/runtime/internal/ansi"
)

type IdleOutcome string

const (
	IdleDone    IdleOutcome = "done"
	IdleBlocked IdleOutcome = "blocked"
	IdleError   IdleOutcome = "error"
)

type IdleClassification struct {
	Outcome IdleOutcome
	Line    string
}

var (
	horizontalSpaceRE = regexp.MustCompile(`[ \t]+`)

	inputPromptRE            = regexp.MustCompile(`(?i)\b(?:y/n|yes/no|do you want)\b|\[[yn]/[yn]\]|\b(?:continue|proceed)\s*\?|\?\s*$`)
	permissionPromptRE       = regexp.MustCompile(`(?i)^\s*[❯›]\s*(?:approve|allow|trust)\b|\b(?:approve|allow|trust)\b.*(?:\?|:)\s*$`)
	confirmationFooterRE     = regexp.MustCompile(`(?i)\bpress\s+enter\s+to\s+(?:confirm|continue|approve|allow)\b|\benter\s+to\s+(?:confirm|continue|approve|allow)\b.*\besc\s+to\s+(?:cancel|go\s+back)\b`)
	claudeTrustQuestionRE    = regexp.MustCompile(`(?i)\bis this a project you created or one you trust\?`)
	claudeTrustChoiceRE      = regexp.MustCompile(`(?i)\b(?:yes,?\s+i trust this folder|no,?\s+exit)\b`)
	promptReasonRE           = regexp.MustCompile(`(?i)^\s*reason\s*:\s*(.+)$`)
	choicePromptRE           = regexp.MustCompile(`(?i)\b(?:which|select|choose)\b.*(?:\?|:)\s*$`)
	numberedChoiceRE         = regexp.MustCompile(`^\s*(?:[>❯›^]\s*)?\d+[.)]\s+\S`)
	selectedNumberedChoiceRE = regexp.MustCompile(`^\s*[>❯›^]\s*\d+[.)]\s+\S`)
	selectedChoiceRE         = regexp.MustCompile(`^\s*[❯›]\s+\S`)
	otherChoiceRE            = regexp.MustCompile(`(?i)^\s*(?:[○◯●◉]|\[[ x]\])\s+\S`)
	errorRE                  = regexp.MustCompile(`(?i)\b(?:error|failed|exception|panic|traceback|fatal)\b`)
	// A provider warning is not a failed turn. Codex prints "⚠ MCP startup
	// incomplete (failed: name)" when an optional MCP server is down and then
	// answers normally; treating that line as the outcome reported a completed
	// turn as failed.
	benignErrorRE = regexp.MustCompile(`(?i)\b(?:0\s+(?:errors?|fail(?:ed|ures?)?)|no\s+(?:errors?|failures?))\b|^\s*⚠|\bMCP (?:startup|client|server)\b`)
	resolutionRE  = regexp.MustCompile(`(?i)\b(?:resolved|recovered|fixed|succeeded|successful|passed|completed|all checks pass|done)\b`)

	workingSpinnerRE = regexp.MustCompile(`(?:…|\.\.\.)\s*\(\s*\d+\s*[hms]`)
	workingFooterRE  = regexp.MustCompile(`(?i)[·•∙]\s*esc\s+to\s+interrupt`)

	codeFenceRE            = regexp.MustCompile("```[^\n]*\n?")
	imageRE                = regexp.MustCompile(`!\[([^]]*)\]\([^)]*\)`)
	linkRE                 = regexp.MustCompile(`\[([^]]+)\]\([^)]*\)`)
	markdownPrefixRE       = regexp.MustCompile(`(?m)^\s{0,3}(?:#{1,6}|>|[-+*]|\d+[.)])\s+`)
	htmlTagRE              = regexp.MustCompile(`<[^>]+>`)
	markdownPunctuationRE  = regexp.MustCompile(`[*_~` + "`" + `]+`)
	allSpaceRE             = regexp.MustCompile(`\s+`)
	leadingBulletRE        = regexp.MustCompile(`^\s*[⏺●•✻✽✶❯›]+\s*`)
	ignoredMirrorSummaryRE = regexp.MustCompile(`(?i)\b(?:esc to interrupt|shift\+tab|for shortcuts|context left|bypass permissions|accept edits)\b`)
)

// ClaudeWorkingFromSnapshot ports the spinner/footer activity detector used by
// the TypeScript daemon. The snapshot should represent the current viewport.
func ClaudeWorkingFromSnapshot(snapshot string) bool {
	if snapshot == "" {
		return false
	}
	clean := ansi.Strip(snapshot)
	if workingSpinnerRE.MatchString(clean) {
		return true
	}
	lines := make([]string, 0)
	for _, line := range strings.Split(clean, "\n") {
		line = strings.TrimRightFunc(line, unicode.IsSpace)
		if line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > 6 {
		lines = lines[len(lines)-6:]
	}
	return workingFooterRE.MatchString(strings.Join(lines, "\n"))
}

// ClassifyIdleReason applies the terminal-tail completion rules.
func ClassifyIdleReason(snapshot string) IdleOutcome { return ClassifySnapshot(snapshot).Outcome }

func ClassifySnapshot(snapshot string) IdleClassification {
	lines := snapshotLines(snapshot)
	// Claude's first-run folder trust dialog is a provider control, not an
	// ordinary assistant question. It can span more lines than the generic
	// terminal-tail window, so recognize the complete dialog before looking at
	// only the trailing controls. Sending a semantic message to this screen can
	// otherwise activate the selected "No, exit" choice and kill the session.
	joined := strings.Join(lines, "\n")
	if claudeTrustQuestionRE.MatchString(joined) && claudeTrustChoiceRE.MatchString(joined) {
		return IdleClassification{
			Outcome: IdleBlocked,
			Line:    "Claude is waiting for you to trust this folder",
		}
	}
	trailing := lines
	if len(trailing) > 12 {
		trailing = trailing[len(trailing)-12:]
	}
	for i := len(trailing) - 1; i >= 0; i-- {
		line := trailing[i]
		if confirmationFooterRE.MatchString(line) {
			for previous := i - 1; previous >= 0; previous-- {
				if match := promptReasonRE.FindStringSubmatch(trailing[previous]); len(match) == 2 {
					return IdleClassification{Outcome: IdleBlocked, Line: displayLine(match[1])}
				}
			}
			return IdleClassification{Outcome: IdleBlocked, Line: displayLine(line)}
		}
		if inputPromptRE.MatchString(line) || permissionPromptRE.MatchString(line) || choicePromptRE.MatchString(line) {
			return IdleClassification{Outcome: IdleBlocked, Line: displayLine(line)}
		}
	}
	numbered := 0
	selectedNumbered := ""
	for _, line := range trailing {
		if numberedChoiceRE.MatchString(line) {
			numbered++
		}
		if selectedNumbered == "" && selectedNumberedChoiceRE.MatchString(line) {
			selectedNumbered = line
		}
	}
	if numbered >= 2 && selectedNumbered != "" {
		prompt := ""
		for i := len(trailing) - 1; i >= 0; i-- {
			line := trailing[i]
			if !numberedChoiceRE.MatchString(line) && (strings.HasSuffix(strings.TrimSpace(line), ":") || strings.HasSuffix(strings.TrimSpace(line), "?")) {
				prompt = line
				break
			}
		}
		if prompt == "" {
			prompt = selectedNumbered
		}
		return IdleClassification{Outcome: IdleBlocked, Line: displayLine(prompt)}
	}
	selected := ""
	for _, line := range trailing {
		if selected == "" && selectedChoiceRE.MatchString(line) {
			selected = line
		}
	}
	if selected != "" {
		for _, line := range trailing {
			if line != selected && otherChoiceRE.MatchString(line) {
				return IdleClassification{Outcome: IdleBlocked, Line: displayLine(selected)}
			}
		}
	}
	for i := len(trailing) - 1; i >= 0; i-- {
		line := trailing[i]
		if !errorRE.MatchString(line) || benignErrorRE.MatchString(line) {
			continue
		}
		resolved := false
		for _, following := range trailing[i+1:] {
			if resolutionRE.MatchString(following) {
				resolved = true
				break
			}
		}
		if resolved {
			return IdleClassification{Outcome: IdleDone}
		}
		return IdleClassification{Outcome: IdleError, Line: displayLine(line)}
	}
	return IdleClassification{Outcome: IdleDone}
}

func snapshotLines(snapshot string) []string {
	clean := stripTerminalControls(snapshot)
	clean = strings.ReplaceAll(clean, "\r", "")
	lines := make([]string, 0)
	trimBorder := func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("│┃║╎╏┆┊", r)
	}
	for _, line := range strings.Split(clean, "\n") {
		line = strings.TrimFunc(line, trimBorder)
		line = horizontalSpaceRE.ReplaceAllString(line, " ")
		if line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return lines
}

func stripTerminalControls(text string) string { return ansi.Strip(text) }

func displayLine(line string) string {
	runes := []rune(line)
	if len(runes) > 180 {
		line = strings.TrimRightFunc(string(runes[:179]), unicode.IsSpace) + "…"
	}
	line = strings.TrimSpace(line)
	line = strings.TrimLeft(line, "❯›")
	return strings.TrimSpace(line)
}

// FinalAssistantSummary returns the concise last assistant text, or an empty
// string when no usable structured event exists.
// finalAssistantText returns the full text of the last assistant message in a
// structured event log, or "" when there is none.
func finalAssistantText(events []json.RawMessage) string {
	for i := len(events) - 1; i >= 0; i-- {
		var event map[string]any
		if json.Unmarshal(events[i], &event) != nil || event["type"] != "assistant" {
			continue
		}
		message, ok := event["message"].(map[string]any)
		if !ok || message["role"] != "assistant" {
			continue
		}
		if text := strings.TrimSpace(assistantContent(message["content"])); text != "" {
			return text
		}
	}
	return ""
}

var (
	askPhraseRE      = regexp.MustCompile(`(?i)\b(?:which|should i|do you want|would you like|do you prefer|shall i|can you confirm|please confirm|let me know|what would you|how would you|which one)\b`)
	optionListLineRE = regexp.MustCompile(`(?m)^\s*(?:\d+[.)]|[a-c][.)]|[-*])\s+\S`)
)

// AssistantQuestion reports whether the last assistant message stopped to ask
// the person something, and returns the question. A structured turn has no
// terminal to read, so this is the only signal that a Rich lane is waiting on
// its manager rather than done. It is deliberately conservative: the closing
// line must end in a question mark, and the message must either ask outright
// (which / should I / do you want / let me know) or lay out options to pick
// from. A rhetorical "?" mid-message does not qualify.
func AssistantQuestion(events []json.RawMessage) (string, bool) {
	text := finalAssistantText(events)
	if text == "" {
		return "", false
	}
	plain := conciseFull(text)
	if !strings.HasSuffix(plain, "?") {
		return "", false
	}
	if !askPhraseRE.MatchString(plain) && !optionListLineRE.MatchString(text) {
		return "", false
	}
	// The question is the last sentence of the last line, read before markdown
	// stripping joins an option list into the sentence before it.
	lines := strings.Split(text, "\n")
	last := ""
	for i := len(lines) - 1; i >= 0 && last == ""; i-- {
		last = conciseFull(lines[i])
	}
	question := last
	if len(question) > 1 {
		if index := strings.LastIndexAny(question[:len(question)-1], ".!?"); index >= 0 && index+1 < len(question) {
			question = strings.TrimSpace(question[index+1:])
		}
	}
	return conciseText(question, 160), true
}

// conciseFull strips markdown like conciseText but keeps the whole text, so
// a trailing question mark can be inspected wherever it sits.
func conciseFull(text string) string {
	text = codeFenceRE.ReplaceAllString(text, " ")
	text = imageRE.ReplaceAllString(text, "$1")
	text = linkRE.ReplaceAllString(text, "$1")
	text = markdownPrefixRE.ReplaceAllString(text, "")
	text = htmlTagRE.ReplaceAllString(text, " ")
	text = markdownPunctuationRE.ReplaceAllString(text, "")
	return strings.TrimSpace(allSpaceRE.ReplaceAllString(text, " "))
}

func FinalAssistantSummary(events []json.RawMessage) string {
	for i := len(events) - 1; i >= 0; i-- {
		var event map[string]any
		if json.Unmarshal(events[i], &event) != nil || event["type"] != "assistant" {
			continue
		}
		message, ok := event["message"].(map[string]any)
		if !ok || message["role"] != "assistant" {
			continue
		}
		text := assistantContent(message["content"])
		if summary := conciseText(text, 120); summary != "" {
			return summary
		}
	}
	return ""
}

func assistantContent(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	blocks, ok := content.([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok || block["type"] != "text" {
			continue
		}
		if text, ok := block["text"].(string); ok {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, " ")
}

func conciseText(text string, maxLength int) string {
	text = codeFenceRE.ReplaceAllString(text, " ")
	text = imageRE.ReplaceAllString(text, "$1")
	text = linkRE.ReplaceAllString(text, "$1")
	text = markdownPrefixRE.ReplaceAllString(text, "")
	text = htmlTagRE.ReplaceAllString(text, " ")
	text = markdownPunctuationRE.ReplaceAllString(text, "")
	text = strings.TrimSpace(allSpaceRE.ReplaceAllString(text, " "))
	if text == "" {
		return ""
	}
	first := text
	for i, r := range text {
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		next := i + utf8.RuneLen(r)
		if next == len(text) || unicode.IsSpace(firstRune(text[next:])) {
			first = text[:next]
			break
		}
	}
	runes := []rune(first)
	if len(runes) <= maxLength {
		return first
	}
	prefix := string(runes[:max(1, maxLength-1)])
	cut := strings.LastIndex(prefix, " ")
	if cut < maxLength*6/10 {
		cut = len(prefix)
	}
	return strings.TrimRightFunc(prefix[:cut], unicode.IsSpace) + "…"
}

func firstRune(value string) rune {
	r, _ := utf8.DecodeRuneInString(value)
	return r
}

func mirrorTailSummary(snapshot string) string {
	lines := snapshotLines(snapshot)
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if !hasLetterOrNumber(line) || ignoredMirrorSummaryRE.MatchString(line) {
			continue
		}
		if summary := conciseText(leadingBulletRE.ReplaceAllString(line, ""), 100); summary != "" {
			return summary
		}
	}
	return ""
}

func hasLetterOrNumber(text string) bool {
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return true
		}
	}
	return false
}
