package session

import (
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// Why a runner is gone.
//
// On 11 September the owner's MacBook rebooted at 10:15. The daemon logged
// `[discover] retired stale runner artifacts for <id>` seven times and the app
// showed seven sessions as "lost" with nothing else — no reason, no time, and
// no indication that the one thing to do was resume them. The daemon knew: it
// can see when the machine started, and a runner whose process began before
// that did not crash and was not ended, the machine went down under it.
//
// The three answers are the three things that actually happen. They are
// deliberately not a guess: a machine that cannot say when it booted reports
// "daemon lost contact", which is exactly what such a daemon knows.
const (
	LostToReboot           = "machine rebooted"
	LostToExit             = "runner exited"
	LostToNoContact        = "daemon lost contact"
	LostStartupUnconfirmed = "runner startup was never confirmed"
)

// lostReason decides between them for one lane. startedAtMS is when the runner
// this record describes began; lastEventAtMS is the last thing the ledger saw.
func lostReason(lane ledger.LaneState, startedAtMS, lastEventAtMS, bootAtMS int64) (string, int64) {
	if lane.RunnerExited {
		at := lane.ClosedAtMS
		if at == 0 {
			at = lastEventAtMS
		}
		return LostToExit, at
	}
	if lane.Created && lane.LaunchStarted && !lane.RunnerReady && !lane.Attached {
		// This is missing startup evidence, not proof that the command never
		// ran. Never turn this into permission to launch a duplicate.
		return LostStartupUnconfirmed, lastEventAtMS
	}
	// A runner that started before this boot cannot have been ended by
	// anything on this boot: the machine restarted under it.
	if bootAtMS > 0 && startedAtMS > 0 && startedAtMS < bootAtMS {
		return LostToReboot, bootAtMS
	}
	return LostToNoContact, lastEventAtMS
}

// needsLostReason is the one definition of "this record reads as lost and does
// not yet say why".
func needsLostReason(info *state.SessionInfo) bool {
	if info.Exited || info.LostReason != "" {
		return false
	}
	return info.RunnerGone || info.UnreachableReason == "runner-lost"
}

// bootAtMS is when this machine started, or zero when it cannot be known.
func bootAtMS() int64 {
	at, ok := state.BootTime()
	if !ok {
		return 0
	}
	return at.UnixMilli()
}

// withLostReason fills in the reason for every record that reads as lost and
// does not have one. It is the one place the rule lives, so a lost session
// looks the same whether the daemon is holding it or rebuilding it from the
// ledger.
func withLostReason(infos []state.SessionInfo, states []ledger.LaneState, boot int64) []state.SessionInfo {
	// Every listing runs this, and most listings have nothing lost in them. One
	// pass with no allocation answers that; the index below is built only when
	// there is something to explain.
	anyLost := false
	for index := range infos {
		if needsLostReason(&infos[index]) {
			anyLost = true
			break
		}
	}
	if !anyLost {
		return infos
	}
	byID := make(map[string]ledger.LaneState, len(states))
	for _, lane := range states {
		byID[lane.LaneID] = lane
	}
	for index := range infos {
		info := &infos[index]
		if !needsLostReason(info) {
			continue
		}
		lastEvent := info.LastDataAt
		if info.UnreachableSince != nil && *info.UnreachableSince > 0 {
			lastEvent = *info.UnreachableSince
		}
		reason, at := lostReason(byID[info.ID], info.CreatedAt, lastEvent, boot)
		info.LostReason = reason
		info.LostAtMS = at
	}
	return infos
}
