package session

import "github.com/somewhere-tech/sessions/runtime/internal/state"

// ExemptFromAutomaticEnd reports whether the automatic machinery must leave a
// session alone. A pin is the user marking a workbench, and the promise it
// makes is that nothing Sessions decides on its own ends it: not the
// task-lifecycle sweep that retires a delegate after a successful final
// response, and not the retention policies that will later end a session for
// sleeping too long.
//
// The judgement lives here rather than at each call site because it is one
// rule, and a rule copied into three places is a rule that will disagree with
// itself. An explicit `sessions kill`, an explicit end from the app, and the
// user's own archive are all deliberate acts by the user and are deliberately
// not consulted here: a pin protects a session from inference, not from its
// owner.
func ExemptFromAutomaticEnd(info state.SessionInfo) bool {
	return info.Pinned
}

// TODO(review): wire this into scheduleTaskCompletion in idle.go. That sweep is
// the one automatic terminator that exists today, and it currently ends a
// completed task session regardless of its pin. The guard belongs immediately
// after the session is re-read there, beside the other conditions that abandon
// the completion:
//
//	if ExemptFromAutomaticEnd(info) {
//		return
//	}
//
// It is not applied from this file because idle.go is being edited on another
// branch and a second writer would collide with it.
