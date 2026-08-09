package session

import "github.com/somewhere-tech/sessions/runtime/internal/state"

// ExemptFromAutomaticEnd reports whether the automatic machinery must leave a
// session alone. A pin is the user marking a workbench, and the promise it
// makes is that nothing Sessions may later decide on its own ends it, including
// a future retention policy for sessions sleeping too long. No automatic
// terminator exists today.
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

// The former task-completion reaper was removed on the owner's rule: lanes are
// never killed automatically. This helper is the standing exemption any future
// automatic machinery (snooze, retention) must consult before acting.
