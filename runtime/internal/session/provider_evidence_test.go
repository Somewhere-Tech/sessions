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
	}, time.Now().UnixMilli())

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
	}, time.Now().UnixMilli())

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
	}, "", time.Now().UnixMilli())

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

// A structured runner's provider_fault event is the provider's own word. It
// carries the line it was read from as evidence, but that line was never on a
// terminal screen, so the scroll-away rule must not retire it: a signed-out
// structured Claude child has to read as auth, not as an ordinary failed turn.
// Structured runners deliver every provider frame as EventCodex.
func TestStructuredProviderFaultSurvivesIdleClassification(t *testing.T) {
	root := t.TempDir()
	launcher := prototest.NewLauncher()
	manager := NewManager(testConfig(root), launcher, ManagerOptions{
		DisableWatchers: true, ActivityInterval: 10 * time.Millisecond,
	})
	t.Cleanup(manager.Close)
	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "claude", Cwd: root, Kind: state.KindClaudeStructured,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := launcher.Runner(created.ID)
	now := time.Now().UTC()
	stamp := func(offset time.Duration) string { return now.Add(offset).Format(time.RFC3339Nano) }
	const source = "claude-p-stream-json"
	runner.AddCodexEvent(map[string]any{"type": "user", "subtype": "user_message", "source": source,
		"session_id": "c", "timestamp": stamp(0), "message": map[string]any{"role": "user", "content": "start"}})
	runner.AddCodexEvent(map[string]any{"type": "claude", "subtype": "turn_started", "source": source,
		"session_id": "c", "timestamp": stamp(time.Millisecond)})
	runner.AddCodexEvent(map[string]any{"type": "system", "subtype": "provider_fault", "provider": "claude",
		"kind": providerfault.KindAuth, "detail": "Claude is not logged in",
		"evidence": "Invalid API key · Please run /login", "timestamp": stamp(2 * time.Millisecond)})
	runner.AddCodexEvent(map[string]any{"type": "result", "subtype": "success", "is_error": true, "source": source,
		"session_id": "c", "result": "Invalid API key · Please run /login", "timestamp": stamp(3 * time.Millisecond)})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if current, _ := manager.Get(created.ID); current.Info().IdleReason == state.IdleReasonFailed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, _ := manager.Get(created.ID)
	info := current.Info()
	if info.IdleReason != state.IdleReasonFailed {
		t.Fatalf("the failed structured turn never settled: idle=%q working=%t", info.IdleReason, info.Working)
	}
	if info.FailureKind != providerfault.KindAuth {
		t.Fatalf("structured auth fault was cleared by idle classification: kind=%q detail=%q", info.FailureKind, info.IdleDetail)
	}
}

// Re-attaching after a daemon restart replays the same fault into a fresh
// session and runs the same sweep, with whatever the runner renders as its
// screen. Neither structured provider's fault may be retired by that screen.
func TestStructuredFaultIsNotRetiredByAnUnrelatedScreen(t *testing.T) {
	for _, test := range []struct{ kind, cmd, provider string }{
		{state.KindClaudeStructured, "claude", "claude"},
		{state.KindCodexAppServer, "codex", "codex"},
	} {
		t.Run(test.kind, func(t *testing.T) {
			root := t.TempDir()
			manager := NewManager(testConfig(root), prototest.NewLauncher(), ManagerOptions{
				DisableWatchers: true, ActivityInterval: time.Hour,
			})
			t.Cleanup(manager.Close)
			created, err := manager.Create(context.Background(), state.CreateSessionRequest{Cmd: test.cmd, Cwd: root, Kind: test.kind})
			if err != nil {
				t.Fatal(err)
			}
			session, _ := manager.registry.Get(created.ID)
			session.SetProviderFault(test.provider, providerfault.Fault{
				Kind: providerfault.KindAuth, Detail: "not logged in", Evidence: "Please run /login",
			}, time.Now().UnixMilli())

			clearFaultWithoutEvidence(session, IdleClassification{Outcome: IdleError}, "")
			if session.Info().FailureKind != providerfault.KindAuth {
				t.Fatalf("%s fault was retired by a screen it was never shown on", test.kind)
			}
		})
	}
}
