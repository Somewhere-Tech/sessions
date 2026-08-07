package session

import (
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// The session shape below is exactly the one scheduleTaskCompletion acts on: a
// delegated task worker that finished cleanly and is idle. That sweep is the
// only automatic terminator in the runtime today, and a pinned session must
// survive it. This test holds the rule; wiring it into idle.go is the TODO in
// pin.go, and this is what will fail loudly if the rule is ever inverted.
func TestAPinnedCompletedTaskIsExemptFromAutomaticEnd(t *testing.T) {
	completedTask := state.SessionInfo{
		ID:         "77777777-8888-4999-8aaa-bbbbbbbbbbbb",
		Lifecycle:  state.LifecycleTask,
		IdleReason: state.IdleReasonCompleted,
	}
	if ExemptFromAutomaticEnd(completedTask) {
		t.Fatal("an unpinned completed task claimed exemption; the aggressive sleep and " +
			"retire policy for delegates is deliberate and must still apply")
	}
	completedTask.Pinned = true
	if !ExemptFromAutomaticEnd(completedTask) {
		t.Fatal("a pinned session was not exempt from automatic termination — pinning is " +
			"the user marking a workbench, and the machinery is supposed to keep its hands off it")
	}
}
