package main

import (
	"fmt"

	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
)

func (a *app) cmdAdopt(args []string) error {
	force := removeFirst(&args, "--force")
	repairLaneID, repairSet := pluck(&args, "--repair")
	sourceSessionID, sourceSet := pluck(&args, "--source")
	if len(args) != 1 || args[0] == "" {
		return fail(1, "usage: sessions adopt <path-or-uuid> [--force] [--source SESSION] [--repair LIVE-SUCCESSOR]")
	}
	var err error
	sourceSessionID, repairLaneID, err = a.resolveRecoverySessionIDs(sourceSessionID, sourceSet, repairLaneID, repairSet)
	if err != nil {
		return err
	}
	body := map[string]any{"target": args[0], "force": force}
	if sourceSet {
		body["sourceSessionId"] = sourceSessionID
	}
	if repairSet {
		body["repairLaneId"] = repairLaneID
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
	if result.Partial {
		fmt.Fprintf(a.stderr, "sessions: %s\n", result.Warning)
		if result.Repair != nil {
			command := []string{"sessions", "adopt", result.Repair.Target, "--repair", result.Repair.LaneID}
			if result.Repair.SourceSessionID != "" {
				command = append(command, "--source", result.Repair.SourceSessionID)
			}
			fmt.Fprintf(a.stderr, "sessions: repair without starting another runtime: %s\n", shellRecipe(command))
		}
	}
	return nil
}
