package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type waitOutcome struct {
	OK      bool   `json:"ok"`
	Kind    string `json:"kind"`
	Reason  string `json:"reason"`
	Session string `json:"session"`
	// Code is the exit status this outcome produces. `sessions help` tells an
	// agent that every --json document carries a code matching the exit status,
	// and this envelope did not: an agent following that instruction read a
	// missing key as zero and a lane that failed with exit 3 came back as
	// success -- the same silent wrong-success that ok was introduced to end.
	// It is never omitempty: zero is the answer that matters most.
	Code      int   `json:"code"`
	Working   bool  `json:"working"`
	IdleMS    int64 `json:"idleMs"`
	ElapsedMS int64 `json:"elapsedMs,omitempty"`
	// Targets is populated only when one outcome covers several targets at
	// once — a race that timed out without any of them answering — because
	// then no single id produced it.
	Targets    []string             `json:"targets,omitempty"`
	IdleReason string               `json:"idleReason,omitempty"`
	Detail     string               `json:"detail,omitempty"`
	Summary    string               `json:"summary,omitempty"`
	Lane       *laneManifest        `json:"lane,omitempty"`
	Condition  *waitConditionDetail `json:"condition,omitempty"`
}

// waitConditionDetail carries what only a --until condition can report.
type waitConditionDetail struct {
	Cwd              string `json:"cwd,omitempty"`
	Baseline         string `json:"baseline,omitempty"`
	Commit           string `json:"commit,omitempty"`
	Subject          string `json:"subject,omitempty"`
	HistoryRewritten bool   `json:"history_rewritten,omitempty"`
	File             string `json:"file,omitempty"`
	Contains         string `json:"contains,omitempty"`
	IdleStableMS     int64  `json:"idle_stable_ms,omitempty"`
	Source           string `json:"source,omitempty"`
}

const (
	waitReasonIdle       = "idle"
	waitReasonNeedsInput = "needs-input"
	waitReasonFailed     = "failed"
	waitReasonGone       = "gone"
	waitReasonTimeout    = "timeout"
	// waitReasonExited is a lane that ran to completion with status 0. A lane
	// that exited non-zero reports failed, which is the same answer a session
	// that ended badly gives, so one branch covers both.
	waitReasonExited = "exited"
	// waitReasonSatisfied is a --until condition that was observed.
	waitReasonSatisfied = "satisfied"
)

const (
	waitKindSession    = "session"
	waitKindLane       = "lane"
	waitKindCommit     = "commit"
	waitKindFile       = "file-contains"
	waitKindIdleStable = "idle-stable"
	// waitKindCondition labels a race between conditions of different kinds
	// that ended without any of them being observed.
	waitKindCondition = "condition"
	// waitKindJoin labels the envelope `wait --all` returns.
	waitKindJoin = "all"
)

// writeWaitOutcome emits the envelope and returns the exit status that matches
// it, so the JSON and the exit code can never disagree.
func (a *app) writeWaitOutcome(outcome waitOutcome, humanText string, humanToStderr bool) error {
	return a.emitWaitOutcome(outcome, humanText, humanToStderr, waitExitStatus(outcome.Reason))
}

// emitWaitOutcome writes the envelope and returns the status the process exits
// with. final is passed in rather than derived, because one caller has a more
// specific status than the reason implies: a single lane wait propagates the
// child's own exit code. Taking it here, and stamping it into the envelope on
// the way out, is what keeps the printed code and the exit status equal by
// construction -- a caller cannot report one and return the other.
func (a *app) emitWaitOutcome(outcome waitOutcome, humanText string, humanToStderr bool, final error) error {
	outcome.Code = statusCode(final)
	if a.wantJSON {
		if err := writeJSON(a.stdout, outcome, false); err != nil {
			return err
		}
		return final
	}
	destination := a.stdout
	if humanToStderr {
		destination = a.stderr
	}
	if _, err := io.WriteString(destination, humanText+"\n"); err != nil {
		return err
	}
	return final
}

// statusCode is the exit status an error stands for, with nil meaning success.
// exitCode alone cannot be used: it reads a nil error as a transport failure,
// because it is only ever reached on a path that already has one.
func statusCode(err error) int {
	if err == nil {
		return exitSatisfied
	}
	return exitCode(err)
}

// waitOutcomeStatus is the exit status one outcome implies on its own, for
// results nested inside a fan-out join, which are never emitted through
// emitWaitOutcome and so never learn a status from it.
func waitOutcomeStatus(outcome waitOutcome) int {
	if outcome.Kind == waitKindLane && outcome.Lane != nil {
		return outcome.Lane.ExitCode
	}
	return statusCode(waitExitStatus(outcome.Reason))
}

func waitExitStatus(reason string) error {
	switch reason {
	case waitReasonTimeout:
		return status(exitWaitTimeout)
	case waitReasonGone, waitReasonFailed, "provider-unavailable", "rate-limited", "auth", "other":
		return status(exitTargetUnavailable)
	default:
		return nil
	}
}

// waitReasonSeverity orders outcomes so a fan-out join can report one aggregate
// reason without hiding the worst thing that happened.
func waitReasonSeverity(reason string) int {
	switch reason {
	case waitReasonGone:
		return 4
	case waitReasonFailed, "provider-unavailable", "rate-limited", "auth", "other":
		return 3
	case waitReasonTimeout:
		return 2
	case waitReasonNeedsInput:
		return 1
	default:
		return 0
	}
}

func (a *app) cmdWait(args []string) error {
	if isWaitUntilArgs(args) {
		return a.cmdWaitUntil(args)
	}
	if len(args) == 0 || args[0] == "" {
		return fail(1, "usage: sessions wait <id> [--idle 2s] [--timeout 30s]")
	}
	idArg := args[0]
	args = args[1:]
	includeSummary := removeFirst(&args, "--summary")
	idle := 2 * time.Second
	timeout := 30 * time.Second
	var err error
	if raw, present := pluck(&args, "--idle"); present && raw != "" {
		idle, err = parseDuration(raw, 0)
		if err != nil {
			return err
		}
	}
	if raw, present := pluck(&args, "--timeout"); present && raw != "" {
		timeout, err = parseDuration(raw, 0)
		if err != nil {
			return err
		}
	}
	if len(args) != 0 {
		return fail(1, "usage: sessions wait <id> [--idle 2s] [--timeout 30s] [--summary]")
	}
	id, err := a.resolveSessionID(idArg)
	if err != nil {
		return err
	}
	start := a.now()
	tracker := waitTracker{ref: waitTargetRef{id: id}}
	for {
		sessions, err := a.listSessions(false)
		if err != nil {
			return err
		}
		probe := a.probeSessionWait(&tracker, sessions, idle, includeSummary)
		if probe.outcome != nil {
			return a.writeWaitOutcome(*probe.outcome, probe.human, probe.humanToStderr)
		}
		if a.now().Sub(start) >= timeout {
			return a.writeWaitOutcome(waitOutcome{
				OK:      false,
				Kind:    waitKindSession,
				Reason:  waitReasonTimeout,
				Session: id,
				Working: probe.working,
				IdleMS:  probe.idleMS,
			}, fmt.Sprintf("timeout: still active after %dms (last %dms ago)",
				timeout.Milliseconds(), probe.idleMS), true)
		}
		a.sleep(waitPollInterval(idle))
	}
}

func waitPollInterval(idle time.Duration) time.Duration {
	poll := idle / 4
	if poll < 100*time.Millisecond {
		poll = 100 * time.Millisecond
	}
	if poll > 500*time.Millisecond {
		poll = 500 * time.Millisecond
	}
	return poll
}

// waitProbe is one target's reading from one poll. A nil outcome means the
// target has not reached a terminal state yet; working and idleMS are still
// reported so the caller can describe a timeout without a second observation.
type waitProbe struct {
	outcome       *waitOutcome
	human         string
	humanToStderr bool
	working       bool
	idleMS        int64
}

// probeSessionWait decides, from one session-list snapshot, whether a session
// target has finished waiting. `wait <id>` and the fan-out join share it so the
// two can never drift into disagreeing about what idle means.
func (a *app) probeSessionWait(tracker *waitTracker, sessions []session, idle time.Duration, includeSummary bool) waitProbe {
	var current *session
	for index := range sessions {
		if sessions[index].ID == tracker.ref.id {
			current = &sessions[index]
			break
		}
	}
	if current == nil {
		// A target that vanished is the outcome a delegating agent most
		// needs to distinguish, and it used to report ok:true and exit 0 —
		// so every loop written as `if rc == 0` treated a dead delegate as
		// a finished one.
		return waitProbe{outcome: &waitOutcome{
			OK:      false,
			Kind:    waitKindSession,
			Reason:  waitReasonGone,
			Session: tracker.ref.id,
		}, human: "gone"}
	}
	if stopped := stoppedSessionWait(*current); stopped != nil {
		return *stopped
	}
	idleFor := time.Duration(0)
	working := current.Working || current.Retry != nil
	if isConfirmableTool(toolOfSession(*current)) {
		if working {
			tracker.notWorkingSince = time.Time{}
		} else if tracker.notWorkingSince.IsZero() {
			tracker.notWorkingSince = a.now()
		}
		if !tracker.notWorkingSince.IsZero() {
			idleFor = a.now().Sub(tracker.notWorkingSince)
		}
	} else {
		base := current.LastDataAt
		if base == 0 {
			base = current.CreatedAt
		}
		idleFor = a.now().Sub(time.UnixMilli(base))
	}
	probe := waitProbe{working: working, idleMS: idleFor.Milliseconds()}
	if idleFor < idle {
		return probe
	}
	summary := current.LastSummary
	if summary == "" {
		summary = current.IdleDetail
	}
	if summary == "" {
		summary = current.IdleReason
	}
	probe.human = fmt.Sprintf("idle for %dms", idleFor.Milliseconds())
	if includeSummary {
		probe.human = fmt.Sprintf("%s — %s", current.ID, summary)
	}
	// --summary now only decides how much prose comes back. It used to
	// decide whether the caller learned which session answered, which
	// made the schema depend on a display flag.
	outcome := waitOutcome{
		OK:         true,
		Kind:       waitKindSession,
		Reason:     waitReasonIdle,
		Session:    current.ID,
		Working:    current.Working,
		IdleMS:     idleFor.Milliseconds(),
		IdleReason: current.IdleReason,
	}
	if includeSummary {
		outcome.Summary = current.LastSummary
	}
	probe.outcome = &outcome
	return probe
}

func stoppedSessionWait(current session) *waitProbe {
	if current.Working || current.Retry != nil || (current.IdleReason != state.IdleReasonNeedsInput && current.IdleReason != state.IdleReasonFailed) {
		return nil
	}
	reason := waitReasonNeedsInput
	if current.IdleReason == state.IdleReasonFailed {
		reason = waitReasonFailed
		if current.FailureKind != "" {
			reason = current.FailureKind
		}
	}
	message := current.LastSummary
	if current.IdleReason == state.IdleReasonNeedsInput && current.IdleDetail != "" {
		message = current.IdleDetail
	}
	if message == "" {
		message = current.IdleReason
	}
	return &waitProbe{outcome: &waitOutcome{
		OK: reason == waitReasonNeedsInput, Kind: waitKindSession, Reason: reason,
		Session: current.ID, Working: false, IdleReason: current.IdleReason,
		Detail: current.IdleDetail, Summary: current.LastSummary,
	}, human: fmt.Sprintf("%s — %s", reason, message), humanToStderr: reason != waitReasonNeedsInput}
}

func positiveInt(raw, label string) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fail(1, "%s must be a positive integer", label)
	}
	return value, nil
}

func executableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}
