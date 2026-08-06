package main

import (
	"encoding/json"
	"testing"
)

// TestStructuredTranscriptIsSharedByBothRunners pins the rendering both
// structured runners answer a SnapshotReq with. It used to be a byte-identical
// copy inside each runner's own snapshot method, so this is the test that keeps
// the two providers' snapshots the same as the format changes.
func TestStructuredTranscriptIsSharedByBothRunners(t *testing.T) {
	history := []json.RawMessage{
		json.RawMessage(`{"message":{"role":"user","content":"hello"}}`),
		json.RawMessage(`{"message":{"role":"assistant","content":[{"type":"text","text":"hi"},{"type":"text","text":" there"}]}}`),
		// Skipped: no message, an unrenderable role, and empty text.
		json.RawMessage(`{"type":"system"}`),
		json.RawMessage(`{"message":{"role":"tool","content":"ignored"}}`),
		json.RawMessage(`{"message":{"role":"user","content":"   "}}`),
		json.RawMessage(`not json`),
	}
	want := "[user]\nhello\n\n[assistant]\nhi there"
	if got := structuredTranscript(history); got != want {
		t.Fatalf("structuredTranscript() = %q, want %q", got, want)
	}

	claude := &claudeStructuredRunner{history: history}
	codex := &codexAppRunner{history: history}
	if claude.snapshot() != want || codex.snapshot() != want {
		t.Fatalf("runner snapshots disagree: claude=%q codex=%q", claude.snapshot(), codex.snapshot())
	}
}
