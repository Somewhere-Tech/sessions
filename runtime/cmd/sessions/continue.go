package main

import (
	"fmt"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func (a *app) cmdContinue(args []string) error {
	force := removeFirst(&args, "--force")
	repairLaneID, repairSet := pluck(&args, "--repair")
	sourceSessionID, sourceSet := pluck(&args, "--source")
	destinationProvider, destinationSet := pluck(&args, "--with")
	terminal := removeFirst(&args, "--terminal")
	structured := removeFirst(&args, "--structured")
	remoteControl := removeFirst(&args, "--remote-control")
	permissions, permissionsSet := pluck(&args, "--permissions")
	if len(args) != 1 || args[0] == "" {
		return fail(1, "usage: sessions resume <name-or-id> [--with claude|codex] [--permissions inherit|constrained|full] [--terminal [--remote-control] | --structured] [--force] [--source SESSION] [--repair LIVE-SUCCESSOR]")
	}
	resolution, err := a.resolveHistoryReference(args[0])
	if err != nil {
		return err
	}
	args[0] = resolution.Reference
	if _, err := a.useQualifiedHistoryReference(&args[0]); err != nil {
		return err
	}
	sourceSessionID, repairLaneID, err = a.resolveRecoverySessionIDs(sourceSessionID, sourceSet, repairLaneID, repairSet)
	if err != nil {
		return err
	}
	if destinationSet && destinationProvider != "claude" && destinationProvider != "codex" {
		return fail(1, "--with must be claude or codex")
	}
	if destinationSet && repairSet {
		return fail(1, "--with cannot be combined with --repair")
	}
	if terminal && repairSet {
		return fail(1, "--terminal cannot be combined with --repair")
	}
	if structured && repairSet {
		return fail(1, "--structured cannot be combined with --repair")
	}
	if terminal && structured {
		return fail(1, "--terminal and --structured cannot be combined")
	}
	if remoteControl && !terminal {
		return fail(1, "--remote-control requires --terminal")
	}
	if remoteControl && destinationSet && destinationProvider != "claude" {
		return fail(1, "--remote-control is available only with Claude")
	}
	permissionMode := ""
	if permissionsSet {
		switch strings.ToLower(strings.TrimSpace(permissions)) {
		case state.PermissionsInherit:
			permissionMode = state.ClaudeChoiceInherit
		case state.PermissionsConstrained:
			permissionMode = state.ClaudePermissionManual
		case state.PermissionsFull:
			permissionMode = state.ClaudePermissionBypass
		default:
			return fail(1, "--permissions must be inherit, constrained, or full")
		}
		if destinationSet && destinationProvider != "claude" {
			return fail(1, "--permissions on resume is currently available only with Claude")
		}
		if repairSet {
			return fail(1, "--permissions cannot be combined with --repair")
		}
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
	if terminal {
		body["runtimeMode"] = "terminal"
	}
	if structured {
		body["runtimeMode"] = "rich"
	}
	if remoteControl {
		body["remoteControl"] = true
	}
	if permissionMode != "" {
		body["claudePermissionMode"] = permissionMode
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
			"sessions: resumed %d authored messages from %s to %s (%s); the source was not modified\n",
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
			command := []string{"sessions", "resume", historyID, "--repair", result.Repair.LaneID}
			if result.Repair.SourceSessionID != "" {
				command = append(command, "--source", result.Repair.SourceSessionID)
			}
			fmt.Fprintf(a.stderr, "sessions: repair without starting another runtime: %s\n", shellRecipe(command))
		}
	}
	return nil
}
