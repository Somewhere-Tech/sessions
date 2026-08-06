package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// waitTargetRef is one thing being waited on. A delegator fanning out does not
// care whether a delegate is an interactive session or a headless lane, so the
// join treats both as targets and only the probe differs.
type waitTargetRef struct {
	id   string
	lane bool
}

// waitTracker carries the per-target state a poll loop has to remember between
// ticks: for a session, when it was last seen working.
type waitTracker struct {
	ref             waitTargetRef
	notWorkingSince time.Time
	last            waitProbe
	outcome         *waitOutcome
	human           string
}

// waitJoinOutcome is what `wait --all` returns. Before it existed, a delegator
// that fanned work out to N delegates had to hand-roll the join: multi-target
// wait refused anything but --any, and --any reports only the first finisher,
// so the remaining N-1 had to be re-waited one at a time with their own
// timeouts, and any of them dying in between was silently indistinguishable
// from one still working.
//
// results is in the order the targets were named, so a caller can zip it
// against the list it fanned out over. ok is true only when every target is ok,
// and reason carries the worst thing that happened so a caller that branches on
// one field still cannot mistake a dead delegate for a finished one.
type waitJoinOutcome struct {
	OK      bool          `json:"ok"`
	Kind    string        `json:"kind"`
	Reason  string        `json:"reason"`
	Waited  int           `json:"waited"`
	Results []waitOutcome `json:"results"`
}

func summarizeWaitJoin(results []waitOutcome) waitJoinOutcome {
	join := waitJoinOutcome{OK: true, Kind: waitKindJoin, Reason: waitReasonIdle, Waited: len(results), Results: results}
	worst := -1
	for _, result := range results {
		if !result.OK {
			join.OK = false
		}
		if severity := waitReasonSeverity(result.Reason); severity > worst {
			worst = severity
			join.Reason = result.Reason
		}
	}
	if len(results) == 0 {
		join.Results = []waitOutcome{}
	}
	return join
}

func (a *app) writeWaitJoin(results []waitOutcome, humanLines []string) error {
	join := summarizeWaitJoin(results)
	if a.wantJSON {
		if err := writeJSON(a.stdout, join, false); err != nil {
			return err
		}
	} else {
		destination := a.stdout
		if !join.OK {
			// The lines still name every target, including the ones that
			// succeeded, so a failed join is readable without a second command.
			destination = a.stderr
		}
		if _, err := io.WriteString(destination, strings.Join(humanLines, "\n")+"\n"); err != nil {
			return err
		}
	}
	return waitExitStatus(join.Reason)
}

// runWaitJoin polls every unfinished target from one snapshot per tick rather
// than one goroutine per target: the daemon sees a single session list per
// interval no matter how wide the fan-out, and every target is judged against
// the same observation.
//
// stopOnFirst turns the join into a race and returns the single target that
// answered first.
func (a *app) runWaitJoin(refs []waitTargetRef, idle, timeout time.Duration, includeSummary, stopOnFirst bool) ([]waitOutcome, []string, error) {
	trackers := make([]*waitTracker, 0, len(refs))
	needSessions := false
	for _, ref := range refs {
		trackers = append(trackers, &waitTracker{ref: ref})
		if !ref.lane {
			needSessions = true
		}
	}
	start := a.now()
	for {
		var sessions []session
		if needSessions {
			listed, err := a.listSessions(false)
			if err != nil {
				return nil, nil, err
			}
			sessions = listed
		}
		pending := 0
		for _, tracker := range trackers {
			if tracker.outcome != nil {
				continue
			}
			if tracker.ref.lane {
				outcome, human, err := a.probeLaneWait(tracker.ref, includeSummary)
				if err != nil {
					return nil, nil, err
				}
				if outcome != nil {
					tracker.outcome, tracker.human = outcome, human
				}
			} else {
				probe := a.probeSessionWait(tracker, sessions, idle, includeSummary)
				tracker.last = probe
				if probe.outcome != nil {
					tracker.outcome, tracker.human = probe.outcome, probe.human
				}
			}
			if tracker.outcome == nil {
				pending++
				continue
			}
			if stopOnFirst {
				return []waitOutcome{*tracker.outcome}, []string{joinLine(tracker)}, nil
			}
		}
		if pending == 0 {
			return collectWaitJoin(trackers)
		}
		if a.now().Sub(start) >= timeout {
			for _, tracker := range trackers {
				if tracker.outcome != nil {
					continue
				}
				kind := waitKindSession
				if tracker.ref.lane {
					kind = waitKindLane
				}
				tracker.outcome = &waitOutcome{
					OK:      false,
					Kind:    kind,
					Reason:  waitReasonTimeout,
					Session: tracker.ref.id,
					Working: tracker.last.working,
					IdleMS:  tracker.last.idleMS,
				}
				tracker.human = fmt.Sprintf("timeout after %dms", timeout.Milliseconds())
			}
			return collectWaitJoin(trackers)
		}
		a.sleep(waitPollInterval(idle))
	}
}

func collectWaitJoin(trackers []*waitTracker) ([]waitOutcome, []string, error) {
	results := make([]waitOutcome, 0, len(trackers))
	lines := make([]string, 0, len(trackers))
	for _, tracker := range trackers {
		results = append(results, *tracker.outcome)
		lines = append(lines, joinLine(tracker))
	}
	return results, lines, nil
}

func joinLine(tracker *waitTracker) string {
	return fmt.Sprintf("%s %s", tracker.ref.id, tracker.human)
}

// probeLaneWait reads one lane's completion manifest. A nil outcome means the
// lane is still running.
func (a *app) probeLaneWait(ref waitTargetRef, includeSummary bool) (*waitOutcome, string, error) {
	manifest, statusCode, err := a.fetchLaneManifest(context.Background(), ref.id)
	if err != nil {
		return nil, "", err
	}
	switch statusCode {
	case http.StatusOK:
		outcome := laneWaitOutcome(ref.id, manifest, includeSummary)
		return &outcome, fmt.Sprintf("exited %d after %s", manifest.ExitCode, formatLaneDuration(manifest.DurationMS)), nil
	case http.StatusConflict:
		return nil, "", nil
	default:
		// A lane the daemon no longer knows is the lane equivalent of a
		// vanished session, and waiting longer cannot help.
		return &waitOutcome{
			OK: false, Kind: waitKindLane, Reason: waitReasonGone, Session: ref.id,
			Detail: fmt.Sprintf("lane manifest returned HTTP %d", statusCode),
		}, "gone", nil
	}
}

// laneWaitOutcome renders a completed lane in the shared envelope. A non-zero
// exit is reported as failed — the same reason a session that ended badly
// gives — so one branch covers both, while the numbers a lane alone can report
// stay in the nested lane object instead of colliding at the top level.
func laneWaitOutcome(id string, manifest laneManifest, includeSummary bool) waitOutcome {
	payload := manifest
	reason := waitReasonExited
	if manifest.ExitCode != 0 {
		reason = waitReasonFailed
	}
	outcome := waitOutcome{
		OK:        manifest.ExitCode == 0,
		Kind:      waitKindLane,
		Reason:    reason,
		Session:   id,
		Working:   false,
		ElapsedMS: manifest.DurationMS,
		Lane:      &payload,
	}
	if includeSummary {
		outcome.Summary = compactSummary(manifest.LastOutputTail)
	}
	return outcome
}
