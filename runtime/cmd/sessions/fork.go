package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
)

func (a *app) cmdFork(args []string) error {
	destinationProvider, destinationSet := pluck(&args, "--with")
	pointValue, pointSet := pluck(&args, "--at")
	messageID, messageIDSet := pluck(&args, "--message-id")
	if len(args) != 1 || args[0] == "" {
		return fail(1, "usage: sessions fork <session> [--with claude|codex] [--at MESSAGE_INDEX [--message-id ID]]")
	}
	if destinationSet && destinationProvider != "claude" && destinationProvider != "codex" {
		return fail(1, "--with must be claude or codex")
	}
	sourceID, err := a.resolveSessionID(args[0])
	if err != nil {
		return err
	}
	body := map[string]any{"sourceSessionId": sourceID}
	if destinationSet {
		body["destinationProvider"] = destinationProvider
	}
	if messageIDSet && !pointSet {
		return fail(1, "--message-id requires --at")
	}
	if pointSet {
		pointIndex, err := strconv.Atoi(strings.TrimSpace(pointValue))
		if err != nil || pointIndex < 0 {
			return fail(1, "--at must be a non-negative message index")
		}
		body["sourceMessageIndex"] = pointIndex
		if messageIDSet {
			body["sourceMessageId"] = strings.TrimSpace(messageID)
		}
	}
	var result recovery.AdoptResult
	if err := a.postJSON("/api/recovery/fork", body, &result, 2); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, result, true)
	}
	if _, err := fmt.Fprintln(a.stdout, result.LaneID); err != nil {
		return err
	}
	fmt.Fprintf(
		a.stderr,
		"sessions: copied %d authored messages into %s; source %s keeps running\n",
		result.ImportedMessages, result.DestinationProvider, result.ForkedFromSessionID,
	)
	return nil
}
