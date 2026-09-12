package api

import (
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// From the owner's recoveries on 11 September: resume was refused because the
// source had a successor, and that successor had itself failed. Resuming the
// failed successor by hand worked — which means the refusal stood between him
// and the only thing that would have.
func TestResumeFollowsTheChainToAFailedSuccessor(t *testing.T) {
	source := state.SessionInfo{ID: "aaaaaaaa-source", ReopenedAs: "bbbbbbbb-successor", Exited: true}
	failed := state.SessionInfo{ID: "bbbbbbbb-successor", Exited: true, ExitCode: intPointer(1)}

	target := resolveResumeTarget([]state.SessionInfo{source, failed}, source, "")

	if target.Refusal != "" {
		t.Fatalf("resume was refused: %s", target.Refusal)
	}
	if target.Session.ID != failed.ID {
		t.Fatalf("resuming %s, want the newest link %s", target.Session.ID, failed.ID)
	}
}

// Two runners on one conversation is how two agents end up writing into one
// transcript. A live successor is opened, not resumed again.
func TestResumeRefusesWhenTheNewestLinkIsRunning(t *testing.T) {
	source := state.SessionInfo{ID: "aaaaaaaa-source", ReopenedAs: "bbbbbbbb-live", Exited: true}
	live := state.SessionInfo{ID: "bbbbbbbb-live"}

	target := resolveResumeTarget([]state.SessionInfo{source, live}, source, "")

	if target.Refusal == "" {
		t.Fatal("a second runner was about to be started on a live conversation")
	}
	// The refusal names exactly what to do next.
	if !strings.Contains(target.Refusal, "bbbbbbbb") || !strings.Contains(target.Refusal, "open it") {
		t.Fatalf("refusal = %q, want it to name the live successor and what to do", target.Refusal)
	}
}

func TestResumeWalksAWholeChain(t *testing.T) {
	first := state.SessionInfo{ID: "11111111-first", ReopenedAs: "22222222-second", Exited: true}
	second := state.SessionInfo{ID: "22222222-second", ReopenedAs: "33333333-third", Exited: true}
	third := state.SessionInfo{ID: "33333333-third", Exited: true}

	target := resolveResumeTarget([]state.SessionInfo{first, second, third}, first, "")

	if target.Session.ID != third.ID {
		t.Fatalf("resuming %s, want the newest %s", target.Session.ID, third.ID)
	}
	if len(target.Followed) != 2 {
		t.Fatalf("followed = %#v, want both links", target.Followed)
	}
}

// A successor that is no longer listed cannot be resumed by this daemon, and
// continuing from the ancestor would fork the conversation. Saying which id to
// ask for is the most that is true.
func TestResumeNamesASuccessorItCannotSee(t *testing.T) {
	source := state.SessionInfo{ID: "aaaaaaaa-source", ReopenedAs: "cccccccc-archived", Exited: true}

	target := resolveResumeTarget([]state.SessionInfo{source}, source, "")

	if target.Refusal == "" {
		t.Fatal("a resume forked the conversation silently")
	}
	if !strings.Contains(target.Refusal, "cccccccc") {
		t.Fatalf("refusal = %q, want the successor named", target.Refusal)
	}
}

// A repair creating its own successor is not in conflict with itself.
func TestResumeIgnoresTheSuccessorItIsCreating(t *testing.T) {
	source := state.SessionInfo{ID: "aaaaaaaa-source", ReopenedAs: "dddddddd-repair", Exited: true}

	target := resolveResumeTarget([]state.SessionInfo{source}, source, "dddddddd-repair")

	if target.Refusal != "" || target.Session.ID != source.ID {
		t.Fatalf("repair refused itself: %#v", target)
	}
}

func intPointer(value int) *int { return &value }
