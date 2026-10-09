package session

import (
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// The MacBook rebooted at 10:15 on 11 September. Seven sessions were retired
// and every one of them read as "lost" with nothing else: no reason, no time,
// and no sign that resuming was the one thing to do.
func TestALostRunnerSaysWhichOfTheThreeThingsHappened(t *testing.T) {
	const boot = 1_757_000_000_000
	for name, test := range map[string]struct {
		lane       ledger.LaneState
		startedAt  int64
		lastEvent  int64
		wantReason string
		wantAt     int64
	}{
		"machine rebooted under it": {
			lane:      ledger.LaneState{},
			startedAt: boot - 3_600_000, lastEvent: boot - 60_000,
			wantReason: LostToReboot, wantAt: boot,
		},
		"runner exited on its own": {
			lane:      ledger.LaneState{RunnerExited: true, ClosedAtMS: boot + 500},
			startedAt: boot + 100, lastEvent: boot + 500,
			wantReason: LostToExit, wantAt: boot + 500,
		},
		"daemon simply lost contact": {
			lane:      ledger.LaneState{},
			startedAt: boot + 1_000, lastEvent: boot + 90_000,
			wantReason: LostToNoContact, wantAt: boot + 90_000,
		},
		"startup never acknowledged": {
			lane:      ledger.LaneState{Created: true, LaunchStarted: true},
			startedAt: boot + 1_000, lastEvent: boot + 90_000,
			wantReason: LostStartupUnconfirmed, wantAt: boot + 90_000,
		},
		"attached runner is not a startup failure": {
			lane:      ledger.LaneState{Created: true, LaunchStarted: true, Attached: true},
			startedAt: boot + 1_000, lastEvent: boot + 90_000,
			wantReason: LostToNoContact, wantAt: boot + 90_000,
		},
		"machine that cannot say when it booted": {
			lane:      ledger.LaneState{},
			startedAt: boot - 3_600_000, lastEvent: boot - 60_000,
			wantReason: LostToNoContact, wantAt: boot - 60_000,
		},
	} {
		machineBoot := int64(boot)
		if name == "machine that cannot say when it booted" {
			machineBoot = 0
		}
		reason, at := lostReason(test.lane, test.startedAt, test.lastEvent, machineBoot)
		if reason != test.wantReason || at != test.wantAt {
			t.Errorf("%s: reason=%q at=%d, want %q at %d", name, reason, at, test.wantReason, test.wantAt)
		}
	}
}

// A record the daemon rebuilt from the ledger and a record it is holding both
// read as lost, and both must carry the reason — the person cannot tell which
// kind they are looking at, and should not have to.
func TestBothKindsOfLostRecordCarryTheReason(t *testing.T) {
	const boot = 1_757_000_000_000
	since := int64(boot - 60_000)
	infos := []state.SessionInfo{
		{ID: "rebuilt", CreatedAt: boot - 7_200_000, Unreachable: true, UnreachableReason: "runner-lost", UnreachableSince: &since},
		{ID: "held", CreatedAt: boot - 7_200_000, RunnerGone: true, LastDataAt: boot - 30_000},
		{ID: "running", CreatedAt: boot + 1_000},
		{ID: "ended", CreatedAt: boot - 7_200_000, Exited: true},
	}

	result := withLostReason(infos, nil, boot)

	if result[0].LostReason != LostToReboot || result[0].LostAtMS != boot {
		t.Errorf("rebuilt record = %q at %d", result[0].LostReason, result[0].LostAtMS)
	}
	if result[1].LostReason != LostToReboot {
		t.Errorf("held record = %q", result[1].LostReason)
	}
	// A session that is fine, and one that ended properly, are not lost and
	// must not acquire a reason for something that did not happen to them.
	if result[2].LostReason != "" || result[3].LostReason != "" {
		t.Errorf("a reason was invented: running=%q ended=%q", result[2].LostReason, result[3].LostReason)
	}
}
