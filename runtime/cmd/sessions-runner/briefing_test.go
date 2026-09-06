package main

import (
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestBriefingInstructionsDoNotReloadFullHistoryOrDuplicateCodexInput(t *testing.T) {
	value := state.ContinuationContext{BriefingOnly: true, SourceProvider: "claude", SourceHistoryID: "original", Messages: []state.ContinuationMessage{{Role: "user", Text: "REVIEWED-CONTEXT"}}}
	claude, codex := continuationBridge(value), codexContinuationInstructions(value)
	for _, instructions := range []string{claude, codex} {
		if strings.Contains(instructions, "sessions --json transcript") || !strings.Contains(instructions, "sessions --json search") {
			t.Fatalf("briefing tries to load the full transcript: %s", instructions)
		}
	}
	if strings.Count(claude, "REVIEWED-CONTEXT") != 1 || strings.Contains(codex, "REVIEWED-CONTEXT") {
		t.Fatal("briefing must reach Claude via the bridge and Codex via imported history, once each")
	}
}
