package main

import (
	"fmt"
	"time"
)

// "Not yet loaded" is not "does not exist".
//
// Reported from the Mini on 11 September: after a restart with 593 sessions the
// daemon spent about three minutes re-attaching runners, and during that window
// `sessions wait <lane>` answered rc 4, "no live session matches" — for a lane
// that existed and was running the whole time. The session was simply not in
// the daemon's registry yet, and nothing in the answer said so.
//
// The daemon now reports its startup phase. Every lookup that would otherwise
// conclude a session is gone asks this first: while the daemon is loading, an
// absent session is one that has not arrived yet, and waiting is the truthful
// thing to do.

// startupWaitBudget bounds a lookup that is waiting for a loading daemon, for
// commands that have no timeout of their own. It is deliberately longer than
// the three minutes the Mini took: a person who is told what is happening and
// can press Ctrl-C is better served by waiting than by a wrong answer.
const startupWaitBudget = 5 * time.Minute

// startupPollInterval is how often a waiting lookup asks again. The daemon is
// at 100% CPU while it loads; this is deliberately unhurried.
const startupPollInterval = 500 * time.Millisecond

type startupState struct {
	Phase     string `json:"phase"`
	Loaded    int    `json:"loaded"`
	Total     int    `json:"total"`
	StartedAt int64  `json:"startedAt"`
}

func (s startupState) loading() bool { return s.Phase == "loading" }

// describe is the one line a person sees while a lookup waits.
func (s startupState) describe() string {
	if s.Total > 0 {
		return fmt.Sprintf("sessionsd is still loading sessions (%d of %d); waiting", s.Loaded, s.Total)
	}
	return "sessionsd is still loading sessions; waiting"
}

// daemonStartup reads the phase from /api/health. A daemon too old to report it,
// or one that cannot be reached, reads as ready: that is what every caller
// assumed before this field existed, and a health probe must never become the
// reason a command fails.
func (a *app) daemonStartup() startupState {
	var health struct {
		Startup startupState `json:"startup"`
	}
	if err := a.getJSON("/api/health", &health); err != nil {
		return startupState{Phase: "ready"}
	}
	if health.Startup.Phase == "" {
		return startupState{Phase: "ready"}
	}
	return health.Startup
}

// waitForLoadingDaemon reports whether the caller should look again.
//
// It prints the notice once per command, so a lookup that polls for three
// minutes says what it is waiting for exactly once, on stderr, where it cannot
// corrupt a --json document.
func (a *app) waitForLoadingDaemon(deadline time.Time) bool {
	state := a.daemonStartup()
	if !state.loading() {
		return false
	}
	if !a.announcedStartupWait {
		a.announcedStartupWait = true
		fmt.Fprintln(a.stderr, "sessions: "+state.describe())
	}
	if a.now().After(deadline) {
		return false
	}
	time.Sleep(startupPollInterval)
	return true
}

// missingSessionError is what to say about a session that is not in the
// listing. While the daemon is loading there is nothing to say yet: the caller
// is told to wait rather than told the session does not exist.
func (a *app) missingSessionError(id string) error {
	if state := a.daemonStartup(); state.loading() {
		return fail(exitTargetUnavailable,
			"%s — this session may not have loaded yet; retry in a few seconds", state.describe())
	}
	return fail(1, "%s", unknownSessionMessage(id))
}

// announceStartupOnce says what the daemon is doing without waiting for it, for
// a command that is going to proceed anyway. A create is safe while the daemon
// re-attaches — it shares no lock with the discovery pass — but on a machine at
// 100% CPU it is slow, and silence is what made it look like a hang.
func (a *app) announceStartupOnce() {
	state := a.daemonStartup()
	if !state.loading() || a.announcedStartupWait {
		return
	}
	a.announcedStartupWait = true
	fmt.Fprintln(a.stderr, "sessions: "+state.describe())
}
