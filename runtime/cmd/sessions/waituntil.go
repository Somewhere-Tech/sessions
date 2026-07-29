package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/waitcond"
)

const waitUntilUsage = "usage: sessions wait <id> --until commit [--timeout 30s] | sessions wait <id> --until-file-contains FILE STRING [--timeout 30s] | sessions wait <id> --until-idle-stable DUR [--timeout 30s] [--any]"

type waitUntilSpec struct {
	kind    waitcond.Kind
	file    string
	literal string
	stable  time.Duration
	session string
}

type waitTarget struct {
	id  string
	cwd string
}

func isWaitUntilArgs(args []string) bool {
	for _, argument := range args {
		switch argument {
		case "--until", "--until-file-contains", "--until-idle-stable", "--any":
			return true
		}
	}
	return false
}

func (a *app) cmdWaitUntil(args []string) error {
	ids, specs, any, timeout, err := parseWaitUntilArgs(args)
	if err != nil {
		return err
	}
	assigned, err := assignWaitSpecs(ids, specs, any)
	if err != nil {
		return err
	}

	conditions := make([]waitcond.Condition, 0, len(assigned))
	for _, spec := range assigned {
		target, err := a.resolveWaitTarget(spec.session)
		if err != nil {
			return err
		}
		var condition waitcond.Condition
		switch spec.kind {
		case waitcond.CommitKind:
			condition, err = waitcond.NewCommit(context.Background(), target.id, target.cwd)
		case waitcond.FileContainsKind:
			condition, err = waitcond.NewFileContains(target.id, target.cwd, spec.file, spec.literal)
		case waitcond.IdleStableKind:
			id := target.id
			condition, err = waitcond.NewIdleStable(id, target.cwd, spec.stable, func(ctx context.Context) (waitcond.IdleSample, error) {
				return a.observeWaitIdle(ctx, id)
			})
		default:
			err = fmt.Errorf("unsupported wait condition %q", spec.kind)
		}
		if err != nil {
			return fail(1, "%s", err)
		}
		conditions = append(conditions, condition)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	started := time.Now()
	result, err := waitcond.WaitAny(ctx, conditions)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return a.writeWaitTimeout(timeout, time.Since(started), len(conditions))
		}
		return fail(1, "%s", err)
	}
	result.Elapsed = time.Since(started)
	return a.writeWaitUntilResult(result)
}

func parseWaitUntilArgs(args []string) ([]string, []waitUntilSpec, bool, time.Duration, error) {
	ids := make([]string, 0, 2)
	specs := make([]waitUntilSpec, 0, 2)
	any := false
	timeout := 30 * time.Second
	timeoutSeen := false
	for index := 0; index < len(args); index++ {
		switch argument := args[index]; argument {
		case "--any":
			if any {
				return nil, nil, false, 0, fail(1, "--any may be specified only once")
			}
			any = true
		case "--timeout":
			if timeoutSeen || index+1 >= len(args) {
				return nil, nil, false, 0, fail(1, "%s", waitUntilUsage)
			}
			timeoutSeen = true
			index++
			var err error
			timeout, err = parseDuration(args[index], 0)
			if err != nil {
				return nil, nil, false, 0, err
			}
			if timeout <= 0 {
				return nil, nil, false, 0, fail(1, "--timeout must be greater than zero")
			}
		case "--until":
			if index+1 >= len(args) || args[index+1] != "commit" {
				return nil, nil, false, 0, fail(1, "--until currently requires 'commit'")
			}
			index++
			specs = append(specs, waitUntilSpec{kind: waitcond.CommitKind})
		case "--until-file-contains":
			if index+2 >= len(args) {
				return nil, nil, false, 0, fail(1, "%s", waitUntilUsage)
			}
			specs = append(specs, waitUntilSpec{
				kind: waitcond.FileContainsKind, file: args[index+1], literal: args[index+2],
			})
			index += 2
		case "--until-idle-stable":
			if index+1 >= len(args) {
				return nil, nil, false, 0, fail(1, "%s", waitUntilUsage)
			}
			stable, err := parseDuration(args[index+1], 0)
			if err != nil {
				return nil, nil, false, 0, err
			}
			if stable <= 0 {
				return nil, nil, false, 0, fail(1, "--until-idle-stable must be greater than zero")
			}
			specs = append(specs, waitUntilSpec{kind: waitcond.IdleStableKind, stable: stable})
			index++
		default:
			if strings.HasPrefix(argument, "-") {
				return nil, nil, false, 0, fail(1, "unknown wait option %s", argument)
			}
			if argument == "" {
				return nil, nil, false, 0, fail(1, "%s", waitUntilUsage)
			}
			ids = append(ids, argument)
		}
	}
	if len(ids) == 0 || len(specs) == 0 {
		return nil, nil, false, 0, fail(1, "%s", waitUntilUsage)
	}
	return ids, specs, any, timeout, nil
}

func assignWaitSpecs(ids []string, specs []waitUntilSpec, any bool) ([]waitUntilSpec, error) {
	var assigned []waitUntilSpec
	switch {
	case len(ids) == 1:
		assigned = append([]waitUntilSpec(nil), specs...)
		for index := range assigned {
			assigned[index].session = ids[0]
		}
	case len(specs) == 1:
		assigned = make([]waitUntilSpec, 0, len(ids))
		for _, id := range ids {
			copy := specs[0]
			copy.session = id
			assigned = append(assigned, copy)
		}
	case len(ids) == len(specs):
		assigned = append([]waitUntilSpec(nil), specs...)
		for index := range assigned {
			assigned[index].session = ids[index]
		}
	default:
		return nil, fail(1, "cannot pair %d session ids with %d conditions", len(ids), len(specs))
	}
	if len(assigned) > 1 && !any {
		return nil, fail(1, "multiple sessions or conditions require --any")
	}
	return assigned, nil
}

func (a *app) resolveWaitTarget(idOrPrefix string) (waitTarget, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	response, err := a.api.request(ctx, http.MethodGet, "/api/sessions?include_exited=1", nil, 0)
	if err == nil && response.status == http.StatusOK {
		var listed sessionsResponse
		if decodeErr := json.Unmarshal(response.body, &listed); decodeErr != nil {
			return waitTarget{}, fail(1, "decode daemon session list: %s", decodeErr)
		}
		candidates := make([]waitTarget, 0, len(listed.Sessions))
		for _, current := range listed.Sessions {
			candidates = append(candidates, waitTarget{id: current.ID, cwd: current.Cwd})
		}
		return selectWaitTarget(idOrPrefix, candidates)
	}

	candidates, metadataErr := a.runnerMetadataTargets()
	if metadataErr != nil {
		return waitTarget{}, fail(1, "read runner metadata: %s", metadataErr)
	}
	return selectWaitTarget(idOrPrefix, candidates)
}

func selectWaitTarget(idOrPrefix string, candidates []waitTarget) (waitTarget, error) {
	for _, candidate := range candidates {
		if candidate.id == idOrPrefix {
			if candidate.cwd == "" {
				return waitTarget{}, fail(1, "session %s has no cwd", candidate.id)
			}
			return candidate, nil
		}
	}
	matches := make([]waitTarget, 0, 2)
	for _, candidate := range candidates {
		if strings.HasPrefix(candidate.id, idOrPrefix) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 1 {
		if matches[0].cwd == "" {
			return waitTarget{}, fail(1, "session %s has no cwd", matches[0].id)
		}
		return matches[0], nil
	}
	if len(matches) == 0 {
		return waitTarget{}, fail(1, "%s", unknownSessionMessage(idOrPrefix))
	}
	return waitTarget{}, fail(1, "ambiguous session prefix '%s' — run `sessions ls`", idOrPrefix)
}

func (a *app) runnerMetadataTargets() ([]waitTarget, error) {
	dir := os.Getenv("SESSIONS_STATE_DIR")
	if dir == "" {
		dir = filepath.Join(a.home, ".local", "state", "sessions", "runners")
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	targets := make([]waitTarget, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		encoded, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var metadata struct {
			ID  string `json:"id"`
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(encoded, &metadata) != nil || metadata.ID == "" {
			continue
		}
		targets = append(targets, waitTarget{id: metadata.ID, cwd: metadata.Cwd})
	}
	return targets, nil
}

func (a *app) observeWaitIdle(ctx context.Context, id string) (waitcond.IdleSample, error) {
	response, err := a.api.request(ctx, http.MethodGet, "/api/sessions/"+escapeID(id)+"/wait", nil, 750*time.Millisecond)
	if err != nil {
		return waitcond.IdleSample{}, err
	}
	if response.status == http.StatusNotFound || response.status == http.StatusConflict {
		return waitcond.IdleSample{}, &waitcond.PreconditionError{Err: fmt.Errorf("session %s is unavailable", id)}
	}
	if response.status >= 400 {
		return waitcond.IdleSample{}, fmt.Errorf("wait observation returned HTTP %d", response.status)
	}
	var observation struct {
		Session string `json:"session"`
		Working bool   `json:"working"`
		Source  string `json:"source"`
	}
	if err := json.Unmarshal(response.body, &observation); err != nil {
		return waitcond.IdleSample{}, err
	}
	if observation.Session != id || (observation.Source != "structured" && observation.Source != "heuristic") {
		return waitcond.IdleSample{}, fmt.Errorf("invalid wait observation for session %s", id)
	}
	return waitcond.IdleSample{Working: observation.Working, Source: observation.Source}, nil
}

func (a *app) writeWaitTimeout(timeout, elapsed time.Duration, count int) error {
	if a.wantJSON {
		_ = writeJSON(a.stdout, struct {
			OK         bool   `json:"ok"`
			Reason     string `json:"reason"`
			ElapsedMS  int64  `json:"elapsed_ms"`
			Conditions int    `json:"conditions"`
		}{false, "timeout", elapsed.Milliseconds(), count}, false)
	} else {
		fmt.Fprintf(a.stderr, "timeout: no condition satisfied after %dms\n", timeout.Milliseconds())
	}
	return status(2)
}

func (a *app) writeWaitUntilResult(result waitcond.Result) error {
	switch result.Kind {
	case waitcond.CommitKind:
		output := struct {
			Session          string `json:"session"`
			Cwd              string `json:"cwd"`
			Baseline         string `json:"baseline"`
			Commit           string `json:"commit"`
			Subject          string `json:"subject"`
			ElapsedMS        int64  `json:"elapsed_ms"`
			HistoryRewritten bool   `json:"history_rewritten"`
		}{result.Session, result.Cwd, result.Baseline, result.Commit, result.Subject, result.Elapsed.Milliseconds(), result.HistoryRewritten}
		if a.wantJSON {
			return writeJSON(a.stdout, output, false)
		}
		rewrite := ""
		if result.HistoryRewritten {
			rewrite = " (history rewritten)"
		}
		_, err := fmt.Fprintf(a.stdout, "%s commit %s %s after %dms%s\n", result.Session, result.Commit, result.Subject, result.Elapsed.Milliseconds(), rewrite)
		return err
	case waitcond.FileContainsKind:
		output := struct {
			Session   string `json:"session"`
			Cwd       string `json:"cwd"`
			File      string `json:"file"`
			Contains  string `json:"contains"`
			ElapsedMS int64  `json:"elapsed_ms"`
		}{result.Session, result.Cwd, result.File, result.Contains, result.Elapsed.Milliseconds()}
		if a.wantJSON {
			return writeJSON(a.stdout, output, false)
		}
		_, err := fmt.Fprintf(a.stdout, "%s observed literal bytes in %s after %dms\n", result.Session, result.File, result.Elapsed.Milliseconds())
		return err
	case waitcond.IdleStableKind:
		output := struct {
			Session      string `json:"session"`
			Cwd          string `json:"cwd"`
			IdleStableMS int64  `json:"idle_stable_ms"`
			ElapsedMS    int64  `json:"elapsed_ms"`
			Source       string `json:"source"`
		}{result.Session, result.Cwd, result.Stable.Milliseconds(), result.Elapsed.Milliseconds(), result.Source}
		if a.wantJSON {
			return writeJSON(a.stdout, output, false)
		}
		_, err := fmt.Fprintf(a.stdout, "%s observed idle for %dms (source: %s)\n", result.Session, result.Stable.Milliseconds(), result.Source)
		return err
	default:
		return io.ErrUnexpectedEOF
	}
}
