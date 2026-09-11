package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/providerfault"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// The founder's session, 11 September: PTY Claude, logged in, working, showing
// NEEDS LOGIN. The agent had gone looking for the code that renders that pill,
// and a row of its grep output — our own authRE source line — arrived in the
// terminal. The generic error rule matched the word "error" in it and the whole
// line was handed to providerfault.Detect, which found "not logged in".
const founderSnapshot = "⏺ Finding where \"needs login\" is detected and rendered\n" +
	"  ⎿  $ cd /Users/uzair/Sessions-wt/reliability-core && grep -rn 'not logged in\\|NEEDS LOGIN\\|needs_login\\|needsLogin\\|Open the terminal to log in' frontend/src runtime/internal\n" +
	"     runtime/internal/providerfault/providerfault.go:31:\tauthRE = regexp.MustCompile(`(?i)\\b(?:not logged in|please run /login|invalid api key|unauthorized|authentication|auth error)\\b`)\n" +
	"     frontend/src/lib/sessionStatus.ts:131:  'auth-needed': 'Needs login',\n" +
	"\n" +
	"✻ Sautéed for 12s · done 4:11 PM\n" +
	"\n" +
	"> \n"

func TestToolOutputAboutLoginIsNotAProviderFault(t *testing.T) {
	classification := ClassifySnapshotFor("claude", founderSnapshot)

	if classification.Evidence.proven() {
		t.Fatalf("the agent's own grep output was taken as proof the provider is logged out: %#v", classification.Evidence)
	}
	// The screen may still read as a failed turn — that is the pre-existing
	// error rule and a different claim. What must not happen is a provider
	// fault: no NEEDS LOGIN pill, no "Claude is not logged in" card.
	if strings.Contains(classification.Line, "not logged in") {
		t.Fatalf("classification line still claims a logged-out provider: %q", classification.Line)
	}
}

// Every class, from the same kind of line: words in tool output prove nothing.
func TestToolOutputNeverProvesRateLimitOrOutage(t *testing.T) {
	for name, row := range map[string]string{
		"rate limit in output": "     anthropic.py:88: raise RateLimitError('rate limit exceeded, try again in 30s')",
		"overloaded in output": "     upstream.log:12: 529 overloaded_error while calling the API",
		"auth in output":       "     Error: MCP server \"fly\": unauthorized — please run /login to continue",
	} {
		snapshot := "⏺ Reading the logs\n  ⎿  $ cat upstream.log\n" + row + "\n\n> \n"
		if evidence := terminalProviderEvidence("claude", terminalRows(snapshot)); evidence.proven() {
			t.Fatalf("%s: tool output was taken as the provider's own words: %#v", name, evidence.Fault)
		}
	}
}

// What does prove it: the provider's own error line, at the left margin, where
// Claude writes its own failures.
func TestClaudeAPIErrorLineIsEvidence(t *testing.T) {
	snapshot := "⏺ Asking Claude\n" +
		"⏺ API Error: 401 {\"type\":\"error\",\"error\":{\"type\":\"authentication_error\"}}\n" +
		"\n> \n"

	evidence := terminalProviderEvidence("claude", terminalRows(snapshot))
	if !evidence.proven() || evidence.Fault.Kind != providerfault.KindAuth {
		t.Fatalf("Claude's own API Error line was not evidence: %#v", evidence)
	}
	// The card shows the line the claim rests on.
	if !strings.Contains(evidence.Fault.Evidence, "API Error: 401") {
		t.Fatalf("evidence line = %q, want the provider's own line", evidence.Fault.Evidence)
	}
}

// The same text, quoted inside a tool result, proves nothing — which is the
// difference between what the provider said and what a program printed.
func TestQuotedAPIErrorInsideToolOutputIsNotEvidence(t *testing.T) {
	snapshot := "⏺ Showing the failing fixture\n" +
		"  ⎿  $ cat testdata/api-error.txt\n" +
		"     ⏺ API Error: 401 {\"type\":\"error\",\"error\":{\"type\":\"authentication_error\"}}\n" +
		"\n> \n"

	if evidence := terminalProviderEvidence("claude", terminalRows(snapshot)); evidence.proven() {
		t.Fatalf("a quoted API Error inside tool output was taken as evidence: %#v", evidence.Fault)
	}
}

// Claude's own login UI, rendered where the person can answer it.
func TestClaudeLoginScreenIsEvidence(t *testing.T) {
	snapshot := "⏺ Starting a turn\n" +
		"\n" +
		"Select login method:\n" +
		"❯ 1. Claude account with subscription\n" +
		"  2. Anthropic Console account\n"

	evidence := terminalProviderEvidence("claude", terminalRows(snapshot))
	if !evidence.proven() || evidence.Fault.Kind != providerfault.KindAuth {
		t.Fatalf("Claude's login screen was not evidence: %#v", evidence)
	}
	if evidence.Fault.Detail != "Claude is not logged in" {
		t.Fatalf("detail = %q", evidence.Fault.Detail)
	}
}

// The same words far up the scrollback are a record of something that already
// happened. Only the live region is a question the person can answer.
func TestLoginPromptInScrollbackIsNotEvidence(t *testing.T) {
	rows := []string{"Please run /login"}
	for range 20 {
		rows = append(rows, "⏺ …and then it kept working")
	}
	snapshot := strings.Join(rows, "\n")

	if evidence := terminalProviderEvidence("claude", terminalRows(snapshot)); evidence.proven() {
		t.Fatalf("a login prompt in scrollback was taken as a live question: %#v", evidence.Fault)
	}
}

// Codex's own anchored error line still classifies; the provider rule is the
// same discipline for every provider, not a Claude special case.
func TestCodexErrorLineIsEvidence(t *testing.T) {
	snapshot := "■ ERROR: stream disconnected before completion\n\n› \n"

	evidence := terminalProviderEvidence("codex", terminalRows(snapshot))
	if !evidence.proven() || evidence.Fault.Kind != providerfault.KindUnavailable {
		t.Fatalf("Codex's own error line was not evidence: %#v", evidence)
	}
}

// A fault must never outlive the evidence for it.
func TestFaultClearsWhenItsEvidenceScrollsAway(t *testing.T) {
	session := newEvidenceSession(t)
	session.SetProviderFault("claude", providerfault.Fault{
		Kind: providerfault.KindAuth, Detail: "Claude is not logged in",
		Evidence: "⏺ API Error: 401 authentication_error",
	}, 100)

	// The screen has moved on: the error line is gone and no login UI replaced
	// it. Nothing supports the claim any more.
	clearFaultWithoutEvidence(session, IdleClassification{Outcome: IdleBlocked},
		"⏺ Working on the next thing\n> \n")
	if session.Info().FailureKind != "" {
		t.Fatalf("fault outlived its evidence: %#v", session.Info().FailureKind)
	}
}

func TestFaultStaysWhileItsEvidenceIsStillOnScreen(t *testing.T) {
	session := newEvidenceSession(t)
	session.SetProviderFault("claude", providerfault.Fault{
		Kind: providerfault.KindAuth, Detail: "Claude is not logged in",
		Evidence: "⏺ API Error: 401 authentication_error",
	}, 100)

	clearFaultWithoutEvidence(session, IdleClassification{Outcome: IdleBlocked},
		"⏺ API Error: 401 authentication_error\n> \n")
	if session.Info().FailureKind == "" {
		t.Fatal("a fault whose line is still on screen was cleared")
	}
}

// applyProviderOutcome is the other half: an error line that is not evidence
// must not become a provider fault, however it reads.
func TestFailedTurnWithoutEvidenceRecordsNoProviderFault(t *testing.T) {
	session := newEvidenceSession(t)

	classification, _ := applyProviderOutcome(session, IdleClassification{
		Outcome: IdleError, Line: "Error: unauthorized while calling the deploy API",
	}, "", 200)

	if session.Info().FailureKind != "" {
		t.Fatalf("a failed turn with no provider evidence recorded %q", session.Info().FailureKind)
	}
	if classification.Outcome != IdleError {
		t.Fatalf("the turn should still read as failed: %#v", classification)
	}
}

// A real session, created through the manager the way every other test in this
// package does, so the fault fields being read are the ones the daemon writes.
func newEvidenceSession(t *testing.T) *state.Session {
	t.Helper()
	root := t.TempDir()
	config := testConfig(root)
	manager := NewManager(config, prototest.NewLauncher(), ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
	})
	t.Cleanup(manager.Close)
	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "claude", Cwd: root, Name: "evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	session, ok := manager.registry.Get(created.ID)
	if !ok {
		t.Fatal("the created session is not registered")
	}
	return session
}
