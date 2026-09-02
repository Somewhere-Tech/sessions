package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// fanoutLane is one provider's lane in a fan-out: which provider answered,
// which session it became, and how its run ended.
type fanoutLane struct {
	Provider string       `json:"provider"`
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	Outcome  *waitOutcome `json:"outcome,omitempty"`
	Error    string       `json:"error,omitempty"`
}

type fanoutReport struct {
	OK      bool             `json:"ok"`
	Request string           `json:"request"`
	Lanes   []fanoutLane     `json:"lanes"`
	Join    *waitJoinOutcome `json:"join,omitempty"`
}

var fanoutProviders = []string{"claude", "codex"}

// cmdFanout gives the same request to one lane per provider and joins them,
// so a change can be checked by an agent from each provider in one step. Run
// from inside a lane the new lanes are its delegated children; from a shell
// they are the person's own sessions. Every lane keeps running afterwards
// and can be opened, questioned, or ended like any other.
func (a *app) cmdFanout(args []string) error {
	with := ""
	if value, present := pluck(&args, "--with"); present {
		with = value
	}
	name := ""
	if value, present := pluck(&args, "--name"); present {
		name = strings.TrimSpace(value)
	}
	cwd := ""
	if value, present := pluck(&args, "--cwd"); present {
		cwd = value
	}
	timeout := 30 * time.Minute
	if raw, present := pluck(&args, "--timeout"); present && raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			return fail(2, "--timeout must be a positive duration like 10m")
		}
		timeout = parsed
	}
	idle := 2 * time.Second
	if raw, present := pluck(&args, "--idle"); present && raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed < 0 {
			return fail(2, "--idle must be a duration like 2s")
		}
		idle = parsed
	}
	noWait := removeFirst(&args, "--no-wait")
	noWorktree := removeFirst(&args, "--no-worktree")
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	request := strings.TrimSpace(strings.Join(args, " "))
	if request == "" {
		return fail(2, "usage: sessions fanout [--with claude,codex] [--name N] [--cwd D] [--timeout D] [--no-wait] [--no-worktree] -- <request...>")
	}

	providers, err := a.fanoutProviderList(with)
	if err != nil {
		return err
	}
	if name == "" {
		name = fanoutName(request)
	}
	report := fanoutReport{Request: request}
	for _, provider := range providers {
		lane := fanoutLane{Provider: provider, Name: name + " (" + provider + ")"}
		newArgs := []string{"--tool", provider, "--name", lane.Name}
		if cwd != "" {
			newArgs = append(newArgs, "--cwd", cwd)
		}
		if noWorktree {
			newArgs = append(newArgs, "--no-worktree")
		}
		newArgs = append(newArgs, request)
		info, createErr := a.createQuietly(newArgs)
		if createErr != nil {
			lane.Error = createErr.Error()
		} else {
			lane.ID, _ = info["id"].(string)
		}
		report.Lanes = append(report.Lanes, lane)
	}

	started := make([]waitTargetRef, 0, len(report.Lanes))
	for _, lane := range report.Lanes {
		if lane.ID != "" {
			started = append(started, waitTargetRef{id: lane.ID})
		}
	}
	if len(started) == 0 {
		report.OK = false
		return a.writeFanout(report, noWait, fail(2, "no lane started: %s", firstFanoutError(report)))
	}
	if noWait {
		report.OK = len(started) == len(report.Lanes)
		return a.writeFanout(report, true, nil)
	}
	results, _, joinErr := a.runWaitJoin(started, idle, timeout, true, false)
	byID := make(map[string]int, len(results))
	for index := range results {
		byID[results[index].Session] = index
	}
	for index := range report.Lanes {
		if position, ok := byID[report.Lanes[index].ID]; ok {
			outcome := results[position]
			report.Lanes[index].Outcome = &outcome
		}
	}
	join := summarizeWaitJoin(results)
	report.Join = &join
	report.OK = joinErr == nil && join.OK && len(started) == len(report.Lanes)
	var final error
	if joinErr != nil {
		final = joinErr
	} else if !report.OK {
		final = fail(join.Code, "%s", fanoutFailure(report))
	}
	return a.writeFanout(report, false, final)
}

func (a *app) fanoutProviderList(with string) ([]string, error) {
	if strings.TrimSpace(with) != "" {
		var chosen []string
		for _, raw := range strings.Split(with, ",") {
			provider := strings.ToLower(strings.TrimSpace(raw))
			if provider == "" {
				continue
			}
			known := false
			for _, candidate := range fanoutProviders {
				if candidate == provider {
					known = true
				}
			}
			if !known {
				return nil, fail(2, "--with accepts claude and codex, not %q", provider)
			}
			chosen = append(chosen, provider)
		}
		if len(chosen) == 0 {
			return nil, fail(2, "--with needs at least one provider")
		}
		return chosen, nil
	}
	var listed struct {
		Providers []providerStatus `json:"providers"`
	}
	if err := a.getJSON("/api/providers", &listed); err != nil {
		// An older daemon without the route: try every provider and let the
		// launch preflight say which one is missing.
		return append([]string(nil), fanoutProviders...), nil
	}
	installed := make([]string, 0, len(fanoutProviders))
	for _, provider := range fanoutProviders {
		for _, status := range listed.Providers {
			if strings.EqualFold(status.ID, provider) && status.Installed {
				installed = append(installed, provider)
			}
		}
	}
	if len(installed) == 0 {
		return nil, fail(2, "no provider is installed; `sessions providers` shows what Sessions can find")
	}
	return installed, nil
}

// createQuietly runs `sessions new` for one lane and returns the session it
// created instead of printing it, so the fan-out can report all lanes at once.
func (a *app) createQuietly(args []string) (map[string]any, error) {
	savedStdout, savedJSON := a.stdout, a.wantJSON
	var captured bytes.Buffer
	a.stdout, a.wantJSON = &captured, true
	err := a.cmdNew(args)
	a.stdout, a.wantJSON = savedStdout, savedJSON
	if err != nil {
		return nil, err
	}
	var info map[string]any
	if decodeErr := json.Unmarshal(captured.Bytes(), &info); decodeErr != nil {
		return nil, fmt.Errorf("sessionsd did not describe the new session: %w", decodeErr)
	}
	return info, nil
}

func fanoutName(request string) string {
	words := strings.Fields(request)
	if len(words) > 6 {
		words = words[:6]
	}
	name := strings.Join(words, " ")
	if len(name) > 48 {
		name = name[:48]
	}
	return strings.TrimSpace(name)
}

func firstFanoutError(report fanoutReport) string {
	for _, lane := range report.Lanes {
		if lane.Error != "" {
			return lane.Provider + ": " + lane.Error
		}
	}
	return "no provider answered"
}

func fanoutFailure(report fanoutReport) string {
	parts := make([]string, 0, len(report.Lanes))
	for _, lane := range report.Lanes {
		switch {
		case lane.Error != "":
			parts = append(parts, lane.Provider+" did not start: "+lane.Error)
		case lane.Outcome != nil && !lane.Outcome.OK:
			parts = append(parts, lane.Provider+" "+lane.Outcome.Reason)
		}
	}
	if len(parts) == 0 {
		return "a lane did not finish cleanly"
	}
	return strings.Join(parts, "; ")
}

func (a *app) writeFanout(report fanoutReport, startedOnly bool, final error) error {
	if a.wantJSON {
		if err := writeJSON(a.stdout, report, true); err != nil {
			return err
		}
		return final
	}
	rows := [][]string{{"PROVIDER", "ID", "STATE", "LAST"}}
	for _, lane := range report.Lanes {
		state, last := "started", "-"
		switch {
		case lane.Error != "":
			state, last = "not started", oneLine(lane.Error)
		case lane.Outcome != nil:
			state = lane.Outcome.Reason
			if lane.Outcome.Summary != "" {
				last = oneLine(lane.Outcome.Summary)
			} else if lane.Outcome.Detail != "" {
				last = oneLine(lane.Outcome.Detail)
			}
		}
		id := "-"
		if lane.ID != "" {
			id = shortID(lane.ID)
		}
		rows = append(rows, []string{lane.Provider, id, state, last})
	}
	if err := writePaddedRows(a.stdout, rows); err != nil {
		return err
	}
	if startedOnly {
		if _, err := io.WriteString(a.stdout, "\nlanes are running; join them with `sessions wait <id>... --all --summary`\n"); err != nil {
			return err
		}
	}
	return final
}
