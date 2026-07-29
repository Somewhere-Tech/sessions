package main

import (
	"fmt"

	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
)

func (a *app) cmdContinue(args []string) error {
	force := removeFirst(&args, "--force")
	repairLaneID, repairSet := pluck(&args, "--repair")
	sourceSessionID, sourceSet := pluck(&args, "--source")
	destinationProvider, destinationSet := pluck(&args, "--with")
	if len(args) != 1 || args[0] == "" {
		return fail(1, "usage: sessions continue <history-id> [--with claude|codex] [--force] [--source SESSION] [--repair LIVE-SUCCESSOR]")
	}
	if repairSet && repairLaneID == "" {
		return fail(1, "--repair requires the existing live successor id")
	}
	if sourceSet && sourceSessionID == "" {
		return fail(1, "--source requires the ended source session id")
	}
	if destinationSet && destinationProvider != "claude" && destinationProvider != "codex" {
		return fail(1, "--with must be claude or codex")
	}
	if destinationSet && repairSet {
		return fail(1, "--with cannot be combined with --repair")
	}
	body := map[string]any{"historyId": args[0], "force": force}
	if sourceSet {
		body["sourceSessionId"] = sourceSessionID
	}
	if repairSet {
		body["repairLaneId"] = repairLaneID
	}
	if destinationSet {
		body["destinationProvider"] = destinationProvider
	}
	var result recovery.AdoptResult
	if err := a.postJSON("/api/recovery/adopt", body, &result, 2); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, result, true)
	}
	if _, err := fmt.Fprintln(a.stdout, result.LaneID); err != nil {
		return err
	}
	if result.Mode != "" {
		fmt.Fprintf(
			a.stderr,
			"sessions: continued %d authored messages from %s to %s (%s); the source was not modified\n",
			result.ImportedMessages, result.SourceProvider, result.DestinationProvider, result.Mode,
		)
	}
	if result.Partial {
		fmt.Fprintf(a.stderr, "sessions: %s\n", result.Warning)
		if result.Repair != nil {
			historyID := result.Repair.HistoryID
			if historyID == "" {
				historyID = args[0]
			}
			command := []string{"sessions", "continue", historyID, "--repair", result.Repair.LaneID}
			if result.Repair.SourceSessionID != "" {
				command = append(command, "--source", result.Repair.SourceSessionID)
			}
			fmt.Fprintf(a.stderr, "sessions: repair without starting another runtime: %s\n", shellRecipe(command))
		}
	}
	return nil
}
