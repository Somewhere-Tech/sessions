package state

import (
	"encoding/json"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/providerfault"
)

func TestStructuredProviderFaultSetsSummaryAndClearsAfterSuccess(t *testing.T) {
	session := &Session{info: SessionInfo{Tool: ToolCodex}}
	fault := proto.Event{Kind: proto.EventCodex, CodexEvent: json.RawMessage(
		`{"type":"system","subtype":"provider_fault","provider":"codex","kind":"provider-unavailable","detail":"Codex API unavailable (503, overloaded)","status":503,"timestamp":"2026-09-03T12:00:00Z"}`,
	)}
	session.recordCodexLocked(&fault)
	info := session.Info()
	if info.FailureKind != "provider-unavailable" || info.FailureProvider != "codex" ||
		info.FailureDetail != "Codex API unavailable (503, overloaded)" || info.LastSummary != info.FailureDetail || info.FailureAt == 0 {
		t.Fatalf("fault projection = %#v", info)
	}
	classified, ok := session.ProviderFault()
	if !ok || classified.Status != 503 || classified.Kind != info.FailureKind {
		t.Fatalf("ProviderFault() = %#v, %v", classified, ok)
	}
	failed := proto.Event{Kind: proto.EventCodex, CodexEvent: json.RawMessage(
		`{"type":"codex","source":"codex-app-server","subtype":"turn_completed","status":"failed"}`,
	)}
	session.recordCodexLocked(&failed)
	if _, ok := session.ProviderFault(); !ok {
		t.Fatal("failed completion cleared the provider fault")
	}
	succeeded := proto.Event{Kind: proto.EventCodex, CodexEvent: json.RawMessage(
		`{"type":"codex","source":"codex-app-server","subtype":"turn_completed","status":"completed"}`,
	)}
	session.recordCodexLocked(&succeeded)
	if info := session.Info(); info.FailureKind != "" || info.FailureDetail != "" || info.FailureProvider != "" || info.FailureAt != 0 {
		t.Fatalf("successful turn retained fault = %#v", info)
	}
}

func TestClaudeSuccessfulResultClearsProviderFault(t *testing.T) {
	session := &Session{info: SessionInfo{Tool: ToolClaude}}
	session.SetProviderFault("claude", providerfault.Fault{Kind: "auth", Detail: "Claude is not logged in"}, 42)
	event := proto.Event{Kind: proto.EventCodex, CodexEvent: json.RawMessage(
		`{"type":"result","source":"claude-p-stream-json","subtype":"success","is_error":false}`,
	)}
	session.recordCodexLocked(&event)
	if _, ok := session.ProviderFault(); ok {
		t.Fatal("successful Claude result retained fault")
	}
}

func TestNativeClaudeAPIErrorProjectsProviderFault(t *testing.T) {
	session := &Session{info: SessionInfo{Tool: ToolClaude}}
	event := proto.Event{Kind: proto.EventClaude, ClaudeEvent: json.RawMessage(
		`{"type":"assistant","isApiErrorMessage":true,"apiErrorStatus":529,"timestamp":"2026-09-03T12:00:00Z","message":{"role":"assistant","content":[{"type":"text","text":"API Error: Repeated 529 Overloaded errors."}]}}`,
	)}
	session.recordClaudeLocked(&event)
	info := session.Info()
	if info.FailureKind != "provider-unavailable" || info.FailureDetail != "Claude API overloaded (529)" ||
		info.FailureProvider != "claude" || info.LastSummary != info.FailureDetail {
		t.Fatalf("native Claude fault = %#v", info)
	}
}
