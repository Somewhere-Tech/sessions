package main

import (
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestWithContinuationSystemPromptPreservesUserPrompt(t *testing.T) {
	args := []string{"--model", "opus", "--append-system-prompt", "User configured prompt."}
	got := withContinuationSystemPrompt(args, "Sessions continuation.")

	if len(got) != len(args) {
		t.Fatalf("len(args) = %d, want %d: %#v", len(got), len(args), got)
	}
	if got[3] != "User configured prompt.\n\nSessions continuation." {
		t.Fatalf("append-system-prompt = %q", got[3])
	}
	if args[3] != "User configured prompt." {
		t.Fatalf("input args mutated: %#v", args)
	}
}

func TestContinuationInstructionsCarryExactSearchableLineage(t *testing.T) {
	continuation := state.ContinuationContext{
		SourceProvider:     "claude",
		SourceHistoryID:    "history-'quoted",
		SourceCWD:          "/work/repo",
		SourceBranch:       "feature/import",
		SourceWorktreePath: "/work/repo-wt",
		SourceRepo:         "/work/repo",
	}

	claude := continuationBridge(continuation)
	for _, fragment := range []string{
		`history ID "history-'quoted"`,
		`'history-'"'"'quoted'`,
		"`sessions --json search <query> --session 'history-'\"'\"'quoted'`",
		`branch "feature/import"`,
		`worktree "/work/repo-wt"`,
	} {
		if !strings.Contains(claude, fragment) {
			t.Fatalf("Claude bridge missing %q: %s", fragment, claude)
		}
	}

	codex := codexContinuationInstructions(continuation)
	for _, fragment := range []string{
		"messages before this point were imported",
		"`sessions --json transcript 'history-'\"'\"'quoted'`",
		"`sessions --json search <query> --session 'history-'\"'\"'quoted'`",
	} {
		if !strings.Contains(codex, fragment) {
			t.Fatalf("Codex instructions missing %q: %s", fragment, codex)
		}
	}
}
