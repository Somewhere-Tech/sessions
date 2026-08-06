package api

import "time"

// See the note in internal/session: these budgets replace a guard on
// t.Context(), which is cancelled only after the test body returns and so
// could never fire.
const (
	waitConditionBudget = 30 * time.Second
	waitConditionPoll   = 2 * time.Millisecond
)
