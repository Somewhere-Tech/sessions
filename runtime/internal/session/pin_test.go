package session

import (
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// No automatic terminator exists today. This pins the standing rule for any
// future cleanup policy: a user-marked live workbench is exempt, while an
// unpinned session is merely eligible for a separately specified policy. This
// helper does not itself decide that an eligible session should end.
func TestAPinnedCompletedTaskIsExemptFromAutomaticEnd(t *testing.T) {
	completedTask := state.SessionInfo{
		ID:         "77777777-8888-4999-8aaa-bbbbbbbbbbbb",
		Lifecycle:  state.LifecycleTask,
		IdleReason: state.IdleReasonCompleted,
	}
	if ExemptFromAutomaticEnd(completedTask) {
		t.Fatal("an unpinned task claimed the user-owned workbench exemption")
	}
	completedTask.Pinned = true
	if !ExemptFromAutomaticEnd(completedTask) {
		t.Fatal("a pinned session lost its standing exemption from future automatic cleanup")
	}
}
