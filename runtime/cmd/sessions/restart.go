package main

import "fmt"

func (a *app) cmdRestart(args []string) error {
	preview := removeFirst(&args, "--preview")
	if preview {
		return a.cmdRestartPreview(args)
	}
	confirm, confirmed := pluck(&args, "--confirm")
	permissions, chosen := pluck(&args, "--permissions")
	remote := removeFirst(&args, "--remote-control")
	terminal := removeFirst(&args, "--terminal")
	structured := removeFirst(&args, "--structured")
	if terminal && structured {
		return fail(1, "--terminal and --structured cannot be combined")
	}
	if len(args) != 1 || !confirmed || !chosen {
		return fail(1, "usage: sessions restart SESSION --confirm EXACT-RUNTIME-ID --permissions constrained|full [--terminal|--structured] [--remote-control]")
	}
	id := args[0]
	var err error
	if id != confirm {
		id, err = a.resolveSessionID(args[0])
	}
	if err != nil {
		return err
	}
	if confirm != id {
		return fail(1, "--confirm must be the exact runtime id %s; inspect sessions status before ending it", id)
	}
	if permissions != "constrained" && permissions != "full" {
		return fail(1, "--permissions must be constrained or full")
	}
	var result struct {
		OK              bool   `json:"ok"`
		Partial         bool   `json:"partial,omitempty"`
		SourceSessionID string `json:"sourceSessionId"`
		SourceEnded     bool   `json:"sourceEnded"`
		LaneID          string `json:"laneId,omitempty"`
		OperationID     string `json:"operationId"`
		Error           string `json:"error,omitempty"`
		Adoption        any    `json:"adoption,omitempty"`
	}
	body := map[string]any{"sourceSessionId": id, "confirmSessionId": confirm, "permissions": permissions, "remoteControl": remote}
	if terminal {
		body["runtimeMode"] = "terminal"
	}
	if structured {
		body["runtimeMode"] = "rich"
	}
	if err := a.postJSON("/api/recovery/restart", body, &result, 2); err != nil {
		return err
	}
	if a.wantJSON {
		if err := writeJSON(a.stdout, result, true); err != nil {
			return err
		}
	} else {
		if result.LaneID != "" {
			fmt.Fprintln(a.stdout, result.LaneID)
		}
		if result.Error != "" {
			fmt.Fprintln(a.stderr, result.Error)
		}
	}
	if !result.OK {
		a.exitCode = 2
	}
	return nil
}

func (a *app) cmdRestartPreview(args []string) error {
	if len(args) != 1 {
		return fail(1, "usage: sessions restart SESSION --preview [--json]")
	}
	id, err := a.resolveSessionID(args[0])
	if err != nil {
		return err
	}
	var result map[string]any
	if err := a.postJSON("/api/recovery/restart/preview", map[string]string{"sourceSessionId": id}, &result, 2); err != nil {
		return err
	}
	return writeJSON(a.stdout, result, true)
}
