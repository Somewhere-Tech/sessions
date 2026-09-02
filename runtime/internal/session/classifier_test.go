package session

import (
	"encoding/json"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestClassifyIdleReasonHeritageSnapshots(t *testing.T) {
	tests := []struct {
		name     string
		snapshot string
		want     IdleOutcome
	}{
		{name: "done screen", snapshot: "Implemented finish notifications.\n12 tests passed, 0 failed.\n❯", want: IdleDone},
		{name: "y/n prompt", snapshot: "The migration changes production data.\nContinue? [y/N]", want: IdleBlocked},
		{name: "codex confirmation footer", snapshot: "Reason: Allow the focused regression test to open tsx's local IPC socket?\nPress enter to confirm or esc to cancel", want: IdleBlocked},
		{name: "numbered picker", snapshot: "Deployment target\n❯ 1) Staging\n  2) Production\n  3) Cancel", want: IdleBlocked},
		{name: "error trace", snapshot: "Traceback (most recent call last):\n  at notify.ts:42\nFatal error: connection failed", want: IdleError},
		{name: "resolved error", snapshot: "Error: first attempt failed\nRetrying with fallback\nAll checks passed", want: IdleDone},
		{name: "codex mcp warning then answer", snapshot: "⚠ MCP client for `somewhere` failed to start: MCP startup failed: handshaking with MCP server failed\n⚠ MCP startup incomplete (failed: somewhere)\n› Reply with exactly AUDIT-FOXTROT.\n• AUDIT-FOXTROT.\n› Write tests for @filename", want: IdleDone},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ClassifyIdleReason(test.snapshot); got != test.want {
				t.Fatalf("ClassifyIdleReason() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestClassifySnapshotUsesCodexApprovalReason(t *testing.T) {
	got := ClassifySnapshot("Reason: Allow the focused regression test to open tsx's local IPC socket?\nPress enter to confirm or esc to cancel")
	if got.Outcome != IdleBlocked || got.Line != "Allow the focused regression test to open tsx's local IPC socket?" {
		t.Fatalf("ClassifySnapshot() = %#v", got)
	}
}

func TestClassifySnapshotRecognizesClaudeFolderTrustDialog(t *testing.T) {
	snapshot := `
Quick safety check: Is this a project you created or one you trust?

Claude Code can read, edit, and execute files in this folder.

  ❯ 1. No, exit
    2. Yes, I trust this folder

Enter to confirm · Esc to cancel
`
	got := ClassifySnapshot(snapshot)
	if got.Outcome != IdleBlocked || got.Line != "Claude is waiting for you to trust this folder" {
		t.Fatalf("ClassifySnapshot() = %#v", got)
	}
}

func TestFinalAssistantSummary(t *testing.T) {
	events := []json.RawMessage{
		json.RawMessage(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"## Shipped **lovable notifications**. Added rich hook metadata too."}]}}`),
		json.RawMessage(`{"type":"assistant","message":{"role":"assistant","content":[]}}`),
	}
	if got := FinalAssistantSummary(events); got != "Shipped lovable notifications." {
		t.Fatalf("FinalAssistantSummary() = %q", got)
	}
}

func TestStructuredIdleClassificationUsesProviderOutcome(t *testing.T) {
	tests := []struct {
		name   string
		kind   string
		event  string
		want   IdleOutcome
		detail string
	}{
		{
			name: "codex completed", kind: state.KindCodexAppServer,
			event: `{"source":"codex-app-server","type":"codex","subtype":"turn_completed","status":"completed"}`,
			want:  IdleDone,
		},
		{
			name: "codex failed", kind: state.KindCodexAppServer,
			event:  `{"source":"codex-app-server","type":"codex","subtype":"turn_completed","status":"failed","error":{"message":"Sandbox setup failed"}}`,
			want:   IdleError,
			detail: "Sandbox setup failed",
		},
		{
			name: "codex waiting on an approval", kind: state.KindCodexAppServer,
			event:  `{"source":"codex-app-server","type":"system","subtype":"approval_requested","approval":{"id":"approval-1","kind":"command","summary":"Run ` + "`npm test`" + `"}}`,
			want:   IdleBlocked,
			detail: "Allow? Run `npm test`",
		},
		{
			name: "claude completed", kind: state.KindClaudeStructured,
			event: `{"source":"claude-p-stream-json","type":"result","subtype":"success","is_error":false}`,
			want:  IdleDone,
		},
		{
			name: "claude failed", kind: state.KindClaudeStructured,
			event:  `{"source":"claude-p-stream-json","type":"result","subtype":"error_during_execution","is_error":true,"result":"Claude command failed"}`,
			want:   IdleError,
			detail: "Claude command failed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := structuredIdleClassification(test.kind, []json.RawMessage{json.RawMessage(test.event)})
			if !ok || got.Outcome != test.want || got.Line != test.detail {
				t.Fatalf("structuredIdleClassification() = %#v, %v; want %q, %q", got, ok, test.want, test.detail)
			}
		})
	}
}

func TestClaudeWorkingFromSnapshot(t *testing.T) {
	tests := []struct {
		name     string
		snapshot string
		want     bool
	}{
		{name: "spinner", snapshot: "✻ Honking… (3m53s · ↓ 15.4k tokens)", want: true},
		{name: "footer", snapshot: "prompt\n(shift+tab to cycle) · esc to interrupt", want: true},
		{name: "prose mention outside footer", snapshot: "esc to interrupt is discussed here\n" + "a\nb\nc\nd\ne\nf\ng", want: false},
		{name: "finished", snapshot: "✻ Cooked for 2m 33s\n· ← for agents", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ClaudeWorkingFromSnapshot(test.snapshot); got != test.want {
				t.Fatalf("ClaudeWorkingFromSnapshot() = %v, want %v", got, test.want)
			}
		})
	}
}

// A Rich lane that ends its turn by asking its manager something is waiting,
// not done. The structured log has no terminal to read, so the last assistant
// message is the signal, and it must actually ask.
func TestStructuredTurnEndingInAQuestionNeedsInput(t *testing.T) {
	assistant := func(text string) string {
		encoded, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": text}})
		return string(encoded)
	}
	codexDone := `{"source":"codex-app-server","type":"codex","subtype":"turn_completed","status":"completed"}`
	claudeDone := `{"source":"claude-p-stream-json","type":"result","subtype":"success","is_error":false}`
	tests := []struct {
		name string
		kind string
		text string
		done string
		want IdleOutcome
		line string
	}{
		{name: "codex asks which", kind: state.KindCodexAppServer, done: codexDone,
			text: "I found two ways to fix the flake. Which one do you want me to take?", want: IdleBlocked, line: "Which one do you want me to take?"},
		{name: "claude offers options", kind: state.KindClaudeStructured, done: claudeDone,
			text: "The migration can go two ways:\n\n1. Rewrite the table in place\n2. Shadow-copy and swap\n\nWhich should I do?", want: IdleBlocked, line: "Which should I do?"},
		{name: "let me know closer", kind: state.KindCodexAppServer, done: codexDone,
			text: "Done. Tests pass. Let me know if you want the changelog too?", want: IdleBlocked, line: "Let me know if you want the changelog too?"},
		{name: "plain completion", kind: state.KindCodexAppServer, done: codexDone,
			text: "Refactored the resolver and added tests. All 14 pass.", want: IdleDone},
		{name: "rhetorical question mid-message", kind: state.KindClaudeStructured, done: claudeDone,
			text: "Why did it fail? The lock was held. I released it and the build is green.", want: IdleDone},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			events := []json.RawMessage{json.RawMessage(assistant(test.text)), json.RawMessage(test.done)}
			got, ok := structuredIdleClassification(test.kind, events)
			if !ok || got.Outcome != test.want || got.Line != test.line {
				t.Fatalf("structuredIdleClassification() = %#v, %v; want %q %q", got, ok, test.want, test.line)
			}
		})
	}
}
