package session

import "time"

// waitConditionBudget bounds every poll-until-true helper in this package's
// tests. These used to guard on t.Context(), which is cancelled only AFTER the
// test function returns -- so the guard could never fire and a condition that
// never became true was an infinite busy wait rather than a failure. It burned
// the whole package timeout and surfaced as an unexplained hang, sometimes in
// another lane's run.
const (
	waitConditionBudget = 30 * time.Second
	waitConditionPoll   = 2 * time.Millisecond
)
