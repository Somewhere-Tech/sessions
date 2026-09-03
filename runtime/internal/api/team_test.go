package api

import (
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func lane(id, parent, tool string) state.SessionInfo {
	return state.SessionInfo{ID: id, ParentSessionID: parent, Tool: state.SessionTool(tool), Name: id}
}

func TestTeamForScopesToParentAndDescendants(t *testing.T) {
	sessions := []state.SessionInfo{
		lane("root", "", "shell"),
		lane("manager", "root", "shell"),
		lane("worker-a", "manager", "codex"),
		lane("worker-b", "manager", "claude"),
		lane("grandchild", "worker-a", "codex"),
		lane("sibling", "root", "shell"), // manager's peer, must not appear
		lane("stranger", "", "shell"),    // unrelated tree
	}
	listing, ok := teamFor(sessions, "manager")
	if !ok {
		t.Fatal("manager not found")
	}
	if listing.Self == nil || listing.Self.ID != "manager" {
		t.Fatalf("self = %#v", listing.Self)
	}
	if listing.Parent == nil || listing.Parent.ID != "root" {
		t.Fatalf("parent = %#v", listing.Parent)
	}
	got := map[string]int{}
	for _, m := range listing.Members {
		got[m.ID] = m.Depth
	}
	for _, unwanted := range []string{"sibling", "stranger", "root", "manager"} {
		if _, present := got[unwanted]; present {
			t.Fatalf("%s must not be a team member: %#v", unwanted, got)
		}
	}
	if got["worker-a"] != 1 || got["worker-b"] != 1 || got["grandchild"] != 2 {
		t.Fatalf("descendant depths = %#v", got)
	}
}

func TestTeamForCountsNeedsInputAndUsesDisplayParent(t *testing.T) {
	blocked := lane("worker", "manager", "codex")
	blocked.IdleReason = state.IdleReasonNeedsInput
	blocked.IdleDetail = "Press enter to continue"
	regrouped := lane("adopted", "someone-else", "shell")
	display := "manager"
	regrouped.DisplayParentSessionID = &display
	sessions := []state.SessionInfo{lane("manager", "", "shell"), blocked, regrouped}
	listing, ok := teamFor(sessions, "manager")
	if !ok {
		t.Fatal("manager not found")
	}
	if listing.NeedsInput != 1 {
		t.Fatalf("needs_input = %d, want 1", listing.NeedsInput)
	}
	ids := map[string]bool{}
	for _, m := range listing.Members {
		ids[m.ID] = true
	}
	if !ids["adopted"] {
		t.Fatalf("display-parent regrouping ignored: %#v", ids)
	}
}

func TestTeamForNamesLostRunnerAndItsRecoveryCommand(t *testing.T) {
	const id = "worker"
	lost := lane(id, "manager", string(state.ToolLane))
	lost.Unreachable = true
	lost.UnreachableReason = "runner-lost"
	lost.RunnerGone = true
	listing, ok := teamFor([]state.SessionInfo{lane("manager", "", "shell"), lost}, "manager")
	if !ok || len(listing.Members) != 1 {
		t.Fatalf("lost team listing = %#v, ok=%v", listing, ok)
	}
	member := listing.Members[0]
	if member.State != "lost" || member.Reason != "runner process is gone" ||
		member.Recovery != "sessions kill "+id || member.Working {
		t.Fatalf("lost team member = %#v", member)
	}
}

func TestTeamForUnknownCallerIsEmptyNotWhole(t *testing.T) {
	sessions := []state.SessionInfo{lane("a", "", "shell"), lane("b", "a", "shell")}
	listing, ok := teamFor(sessions, "nobody")
	if ok {
		t.Fatal("unknown caller should not resolve")
	}
	if len(listing.Members) != 0 || listing.Self != nil {
		t.Fatalf("unknown caller listing = %#v", listing)
	}
}

func TestTruncateBudgetRuneSafe(t *testing.T) {
	if got := truncateBudget("hello", 200); got != "hello" {
		t.Fatalf("short value changed: %q", got)
	}
	long := ""
	for i := 0; i < 300; i++ {
		long += "é"
	}
	got := truncateBudget(long, teamSummaryBudget)
	if len([]byte(got)) > teamSummaryBudget+len("…") {
		t.Fatalf("over budget: %d bytes", len(got))
	}
}
