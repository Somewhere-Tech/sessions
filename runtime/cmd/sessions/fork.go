package main

import (
	"fmt"

	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
)

func (a *app) cmdFork(args []string) error {
	destinationProvider, destinationSet := pluck(&args, "--with")
	if len(args) != 1 || args[0] == "" {
		return fail(1, "usage: sessions fork <live-session> [--with claude|codex]")
	}
	if destinationSet && destinationProvider != "claude" && destinationProvider != "codex" {
		return fail(1, "--with must be claude or codex")
	}
	body := map[string]any{"sourceSessionId": args[0]}
	if destinationSet {
		body["destinationProvider"] = destinationProvider
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
