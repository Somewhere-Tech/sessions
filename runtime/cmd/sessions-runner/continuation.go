package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func continuationTime(value string) time.Time {
	if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value)); err == nil {
		return parsed
	}
	return time.Now()
}

func continuationBridge(continuation state.ContinuationContext) string {
	opening := fmt.Sprintf(
		"You are continuing work from a %s conversation selected by the user in Sessions.",
		continuation.SourceProvider,
	)
	if continuation.Fork {
		opening = fmt.Sprintf(
			"You are working in a new branch copied from a live %s conversation in Sessions. "+
				"The original conversation remains live and may continue independently.",
			continuation.SourceProvider,
		)
	}
	return fmt.Sprintf(
		"%s The exact authored history remains local and searchable as history ID %q. "+
			"Before answering the first request, load it with `sessions --json transcript %s`; "+
			"for a narrower lookup, use `sessions --json search <query> --session %s`. "+
			"Treat user and assistant messages as prior conversation context. Tool output, credentials, "+
			"and provider-internal records were deliberately not imported. %s",
		opening,
		continuation.SourceHistoryID,
		shellQuoted(continuation.SourceHistoryID),
		shellQuoted(continuation.SourceHistoryID),
		continuationWorkspaceDetail(continuation),
	)
}

func codexContinuationInstructions(continuation state.ContinuationContext) string {
	opening := fmt.Sprintf(
		"This Codex conversation was explicitly continued by the user from a %s conversation in Sessions.",
		continuation.SourceProvider,
	)
	if continuation.Fork {
		opening = fmt.Sprintf(
			"This Codex conversation is a new branch copied from a live %s conversation in Sessions. "+
				"The original conversation remains live and may continue independently.",
			continuation.SourceProvider,
		)
	}
	return fmt.Sprintf(
		"%s The authored user and assistant messages before this point were imported into model-visible history. "+
			"The unchanged source remains locally searchable as history ID %q. If you need an omitted tool result "+
			"or a narrower section of a long conversation, use `sessions --json transcript %s` or "+
			"`sessions --json search <query> --session %s`. Tool output, credentials, attachments, and "+
			"provider-internal records were deliberately not imported. %s",
		opening,
		continuation.SourceHistoryID,
		shellQuoted(continuation.SourceHistoryID),
		shellQuoted(continuation.SourceHistoryID),
		continuationWorkspaceDetail(continuation),
	)
}

func continuationWorkspaceDetail(continuation state.ContinuationContext) string {
	parts := []string{fmt.Sprintf("The source workspace was %q", continuation.SourceCWD)}
	if continuation.SourceBranch != "" {
		parts = append(parts, fmt.Sprintf("branch %q", continuation.SourceBranch))
	}
	if continuation.SourceWorktreePath != "" {
		parts = append(parts, fmt.Sprintf("worktree %q", continuation.SourceWorktreePath))
	}
	if continuation.SourceRepo != "" {
		parts = append(parts, fmt.Sprintf("repository %q", continuation.SourceRepo))
	}
	return strings.Join(parts, "; ") + "."
}

func withContinuationSystemPrompt(args []string, prompt string) []string {
	result := append([]string(nil), args...)
	for index := 0; index+1 < len(result); index++ {
		if result[index] != "--append-system-prompt" {
			continue
		}
		result[index+1] = strings.TrimSpace(result[index+1]) + "\n\n" + prompt
		return result
	}
	return append(result, "--append-system-prompt", prompt)
}

func shellQuoted(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func continuationHistoryID(value *state.ContinuationContext) string {
	if value == nil {
		return ""
	}
	return value.SourceHistoryID
}

func continuationSourceProvider(value *state.ContinuationContext) string {
	if value == nil {
		return ""
	}
	return value.SourceProvider
}

func continuationMode(value *state.ContinuationContext) string {
	if value == nil {
		return ""
	}
	return value.Mode
}

func continuationMessageCount(value *state.ContinuationContext) int {
	if value == nil {
		return 0
	}
	return len(value.Messages)
}
