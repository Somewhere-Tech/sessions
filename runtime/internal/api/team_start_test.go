package api

import (
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestTeamStartCountsBlockedAuthWithoutIdleInference(t *testing.T) {
	worker := lane("worker", "manager", "claude")
	worker.FailureKind, worker.FailureDetail = "auth", "Connect the account"
	worker.Working = true // A stale activity hint must not hide the provider fault.
	worker.Start = &state.StartReceipt{OperationID: "5b000000-0000-4000-8000-000000000001"}
	infos := []state.SessionInfo{lane("manager", "", "shell"), worker}
	listing, _ := teamFor(infos, "manager")
	server := &Server{}
	server.attachTeamStarts(&listing, infos)
	member := listing.Members[0]
	if listing.NeedsInput != 1 || !member.NeedsYou || member.Working || member.State != "failed" || member.Start == nil || member.Start.Phase != state.StartPhaseBlocked {
		t.Fatalf("blocked member = %+v", listing)
	}
	if !strings.Contains(member.Waiting, "Connect") || member.Start.Recovery.Action != state.StartRecoveryConnectAccount {
		t.Fatalf("missing account recovery: %+v", member)
	}
}

func TestTeamDeltaIncludesPromptDeliveryProgress(t *testing.T) {
	var cache teamChanges
	first := teamListing{Members: []teamMember{{ID: "worker", Start: &state.StartReceipt{Phase: state.StartPhaseCreated}}}}
	if err := cache.apply("manager", "", &first, time.Now()); err != nil {
		t.Fatal(err)
	}
	next := teamListing{Members: []teamMember{{ID: "worker", Start: &state.StartReceipt{Phase: state.StartPhasePromptDelivered}}}}
	if err := cache.apply("manager", first.NextCursor, &next, time.Now()); err != nil || len(next.Members) != 1 {
		t.Fatalf("delivery change lost: %+v %v", next, err)
	}
}
