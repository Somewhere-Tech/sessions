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

	sessionstate "github.com/somewhere-tech/sessions/runtime/internal/state"
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
	waited := make([]string, 0, len(assigned))
	kind := ""
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
			var path string
			path, err = resolveWaitFilePath(spec.file)
			if err == nil {
				condition, err = waitcond.NewFileContains(target.id, target.cwd, path, spec.literal)
			}
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
		waited = append(waited, target.id)
		specKind := waitConditionKindLabel(spec.kind)
		if kind == "" {
			kind = specKind
		} else if kind != specKind {
			kind = waitKindCondition
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	started := time.Now()
	result, err := waitcond.WaitAny(ctx, conditions)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return a.writeWaitTimeout(kind, uniqueStrings(waited), timeout, time.Since(started))
		}
		return fail(1, "%s", err)
	}
	result.Elapsed = time.Since(started)
	return a.writeWaitUntilResult(result)
}

func waitConditionKindLabel(kind waitcond.Kind) string {
	switch kind {
	case waitcond.CommitKind:
		return waitKindCommit
	case waitcond.FileContainsKind:
		return waitKindFile
	case waitcond.IdleStableKind:
		return waitKindIdleStable
	default:
		return waitKindCondition
	}
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		unique = append(unique, value)
	}
	return unique
}

// resolveWaitFilePath roots a relative --until-file-contains path at the
// caller's working directory, not the delegate's.
//
// The delegate's cwd was the old rule, and it is a directory the caller usually
// does not know: `sessions new` defaults a session's cwd to $HOME while
// `sessions run` inherits the caller's, so the same relative path meant
// different files depending on how the target was created. The condition then
// watched a path that would never exist and the wait simply timed out with
// nothing to explain it. A relative path now means what it means in the shell
// that typed it, and an absolute path is passed through untouched.
func resolveWaitFilePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fail(1, "--until-file-contains needs a file path")
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	working, err := os.Getwd()
	if err != nil {
		return "", fail(1, "resolve working directory for '%s': %s — pass an absolute path instead", path, err)
	}
	return filepath.Join(working, path), nil
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
	ids := make([]idCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, labeledID(candidate.id, candidate.cwd))
	}
	id, found, err := resolveIDPrefix(idOrPrefix, "session", "sessions ls", ids)
	if err != nil {
		return waitTarget{}, err
	}
	if !found {
		return waitTarget{}, fail(1, "%s", unknownSessionMessage(idOrPrefix))
	}
	matched := candidates[candidateIndex(id, ids)]
	if matched.cwd == "" {
		return waitTarget{}, fail(1, "session %s has no cwd", matched.id)
	}
	return matched, nil
}

func (a *app) runnerMetadataTargets() ([]waitTarget, error) {
	// Mirror state.stateRootsFromEnv: SESSIONS_STATE_DIR *is* the runner
	// directory, and its absence falls back to <user state root>/runners. The
	// root is derived, never spelled out, so this resolves on Windows too.
	dir := os.Getenv("SESSIONS_STATE_DIR")
	if dir == "" {
		dir = filepath.Join(sessionstate.UserStateRootFor(a.home), "runners")
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

// writeWaitTimeout reports a wait that ended without observing anything, in the
// same envelope every other wait answers with. It used to emit its own object
// whose only fields were ok, reason, elapsed_ms, and a condition count — no
// target id at all, so a caller racing several delegates learned that something
// timed out but never which one.
func (a *app) writeWaitTimeout(kind string, targets []string, timeout, elapsed time.Duration) error {
	outcome := waitOutcome{
		OK: false, Kind: kind, Reason: waitReasonTimeout, ElapsedMS: elapsed.Milliseconds(),
	}
	switch len(targets) {
	case 0:
	case 1:
		outcome.Session = targets[0]
	default:
		// A race that nothing won has no single answering target, so every
		// target that was being watched is named instead.
		outcome.Targets = targets
	}
	// A timeout is not a transport failure. This branch used to share exit
	// code 2 with "the daemon could not be reached", so a caller could not
	// tell a slow target from a broken connection.
	return a.emitWaitOutcome(outcome,
		fmt.Sprintf("timeout: no condition satisfied after %dms", timeout.Milliseconds()), true,
		status(exitWaitTimeout))
}

func (a *app) writeWaitUntilResult(result waitcond.Result) error {
	outcome := waitOutcome{
		OK: true, Reason: waitReasonSatisfied, Session: result.Session,
		ElapsedMS: result.Elapsed.Milliseconds(),
		Condition: &waitConditionDetail{Cwd: result.Cwd},
	}
	human := ""
	switch result.Kind {
	case waitcond.CommitKind:
		outcome.Kind = waitKindCommit
		outcome.Condition.Baseline = result.Baseline
		outcome.Condition.Commit = result.Commit
		outcome.Condition.Subject = result.Subject
		outcome.Condition.HistoryRewritten = result.HistoryRewritten
		rewrite := ""
		if result.HistoryRewritten {
			rewrite = " (history rewritten)"
		}
		human = fmt.Sprintf("%s commit %s %s after %dms%s", result.Session, result.Commit, result.Subject, result.Elapsed.Milliseconds(), rewrite)
	case waitcond.FileContainsKind:
		outcome.Kind = waitKindFile
		outcome.Condition.File = result.File
		outcome.Condition.Contains = result.Contains
		human = fmt.Sprintf("%s observed literal bytes in %s after %dms", result.Session, result.File, result.Elapsed.Milliseconds())
	case waitcond.IdleStableKind:
		outcome.Kind = waitKindIdleStable
		outcome.Condition.IdleStableMS = result.Stable.Milliseconds()
		outcome.Condition.Source = result.Source
		outcome.IdleMS = result.Stable.Milliseconds()
		human = fmt.Sprintf("%s observed idle for %dms (source: %s)", result.Session, result.Stable.Milliseconds(), result.Source)
	default:
		return io.ErrUnexpectedEOF
	}
	return a.emitWaitOutcome(outcome, human, false, nil)
}
