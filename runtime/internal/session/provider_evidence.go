package session

import (
	"regexp"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/ansi"
	"github.com/somewhere-tech/sessions/runtime/internal/providerfault"
)

// What proves a provider fault, and what merely mentions one.
//
// Reported by the founder on 11 September: a PTY Claude session that was logged
// in and working showed NEEDS LOGIN and "Claude is not logged in — Open the
// terminal to log in". The session's own event stream says what happened. The
// agent had run
//
//	⏺ Finding where "needs login" is detected and rendered
//	  ⎿  $ … grep -rn 'not logged in\|NEEDS LOGIN\|needs_login\|…' frontend/src runtime/internal
//
// and a row of that grep's output — our own `authRE` source line, which
// contains both "auth error" and "not logged in" — arrived in the terminal.
// ClassifySnapshot's generic error rule matched the word "error" in it, and
// idle.go then handed the whole line to providerfault.Detect, which found the
// words it looks for. An agent searching for the string that renders this claim
// made Sessions render the claim.
//
// Everything a provider says and everything a tool prints flow through the same
// PTY, so words alone can never carry this. A fault is now raised only from
// evidence: a line the provider itself rendered, in its own error or login UI,
// outside any tool result. Assistant prose, echoed commands and tool output are
// never evidence, whatever they contain.

var (
	// Claude prints its own failures at the start of a row, as ⏺ API Error.
	// Anything indented under a ⎿ is output it is showing you, not something it
	// is telling you.
	claudeToolResultRE = regexp.MustCompile(`^\s*(?:⎿|⏵|└|│\s*⎿)`)
	// Claude's own login UI, rendered where the person can answer it.
	claudeLoginScreenRE = regexp.MustCompile(
		`(?i)^(?:select login method|sign in to claude|log in to claude|claude code can now be used with your claude subscription)\b`)
	claudeLoginPromptRE = regexp.MustCompile(`(?i)^(?:please )?(?:run |type )?/login\b|^please run /login\b`)
	// How far back "rendered at the cursor" reaches. Claude's login screen is a
	// few rows tall and always sits at the bottom of the viewport; a /login
	// prompt further up is scrollback, which is history, not a question.
	liveRegionRows = 12
)

// providerEvidence is the provider's own words, with the rendered line they came
// from. The zero value means the terminal proves nothing.
type providerEvidence struct {
	Fault providerfault.Fault
	// Line is what the claim rests on. It is shown to the person, because a
	// claim about their provider that cannot be traced to a line on their own
	// screen is exactly the claim this file exists to stop.
	Line string
}

func (e providerEvidence) proven() bool { return e.Fault.Kind != "" }

// terminalRows renders a snapshot as rows with their indentation intact.
// snapshotLines trims each row for prose matching; indentation is what
// distinguishes Claude's own output from the tool output it is displaying, so
// the evidence scan cannot use the trimmed form.
func terminalRows(snapshot string) []string {
	clean := strings.ReplaceAll(ansi.Strip(snapshot), "\r", "")
	rows := strings.Split(clean, "\n")
	for index, row := range rows {
		rows[index] = strings.TrimRight(row, " \t")
	}
	return rows
}

// terminalProviderEvidence is the only way a PTY session acquires a provider
// fault. It scans the rendered rows for the provider's own error line or its
// login UI, and refuses everything printed inside a tool result.
func terminalProviderEvidence(provider string, rows []string) providerEvidence {
	if evidence := claudeLoginEvidence(provider, rows); evidence.proven() {
		return evidence
	}
	inToolResult := toolResultRows(rows)
	for index := len(rows) - 1; index >= 0; index-- {
		row := rows[index]
		if strings.TrimSpace(row) == "" || inToolResult[index] {
			continue
		}
		line := strings.TrimSpace(row)
		if benignErrorRE.MatchString(line) {
			continue
		}
		lineProvider, candidate := terminalProviderFaultLine(line)
		if !candidate {
			continue
		}
		fault, matched := providerfault.Detect(lineProvider, line, 0)
		if !matched {
			continue
		}
		if resolvedAfter(rows[index+1:]) {
			return providerEvidence{}
		}
		fault.Evidence = displayLine(line)
		return providerEvidence{Fault: fault, Line: displayLine(line)}
	}
	return providerEvidence{}
}

// toolResultRows marks the rows that belong to output the agent is displaying
// rather than to the provider. A ⎿ opens a tool result; every indented row
// under it continues the same result; a row at the left margin ends it. The
// pass runs top-down because that is the direction the block structure reads —
// the opener is always above its continuation rows.
func toolResultRows(rows []string) []bool {
	inside := make([]bool, len(rows))
	current := false
	for index, row := range rows {
		switch {
		case claudeToolResultRE.MatchString(row):
			current = true
		case strings.TrimSpace(row) == "":
			// A blank row neither opens nor closes a block.
		case !indented(row):
			current = false
		}
		inside[index] = current
	}
	return inside
}

func indented(row string) bool {
	return row != strings.TrimLeft(row, " \t")
}

func resolvedAfter(following []string) bool {
	for _, row := range following {
		line := strings.TrimSpace(row)
		if claudeTurnFooterRE.MatchString(line) {
			continue
		}
		if resolutionRE.MatchString(line) {
			return true
		}
	}
	return false
}

// claudeLoginEvidence recognizes Claude's own login UI where the person can
// answer it: in the live region at the bottom of the viewport, at the left
// margin, outside any tool result. The same words in scrollback are a record of
// something that already happened, and the same words inside a tool result are
// somebody else's program talking.
func claudeLoginEvidence(provider string, rows []string) providerEvidence {
	if providerfault.Canonical(provider) != "claude" {
		return providerEvidence{}
	}
	start := max(0, len(rows)-liveRegionRows)
	inToolResult := toolResultRows(rows)
	for index := len(rows) - 1; index >= start; index-- {
		row := rows[index]
		if strings.TrimSpace(row) == "" || inToolResult[index] || indented(row) {
			continue
		}
		line := strings.TrimSpace(row)
		if !claudeLoginScreenRE.MatchString(line) && !claudeLoginPromptRE.MatchString(line) {
			continue
		}
		return providerEvidence{
			Fault: providerfault.Fault{
				Kind: providerfault.KindAuth, Detail: "Claude is not logged in", Evidence: displayLine(line),
			},
			Line: displayLine(line),
		}
	}
	return providerEvidence{}
}

// evidenceStillOnScreen reports whether the line a fault was raised from is
// still rendered. A fault that has lost its evidence and shows no login UI has
// nothing left to stand on, which is how a claim about the provider stops being
// made without waiting for the next completed turn.
func evidenceStillOnScreen(provider, evidence string, snapshot string) bool {
	if strings.TrimSpace(evidence) == "" {
		// Nothing to check: faults recorded before evidence was carried, and
		// structured faults, are left alone rather than cleared on a guess.
		return true
	}
	rows := terminalRows(snapshot)
	if claudeLoginEvidence(provider, rows).proven() {
		return true
	}
	for _, row := range rows {
		if strings.Contains(displayLine(strings.TrimSpace(row)), evidence) {
			return true
		}
	}
	return false
}
