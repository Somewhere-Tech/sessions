package state

import "errors"

var (
	ErrSessionNotFound = errors.New("session not found")
	ErrSessionEnded    = errors.New("session has ended")
	// ErrNoPendingApproval: nothing is waiting for a decision on this session.
	ErrNoPendingApproval = errors.New("nothing is waiting for approval")
	ErrSessionWorking    = errors.New("session is working")
	ErrRunnerProtocol    = errors.New("runner protocol does not support this operation")
	ErrRetryUnsupported  = errors.New("provider retry is available only for Rich Claude or Codex sessions")
	ErrNoFailedTurn      = errors.New("nothing failed, so there is no provider turn to retry")
	ErrNoRetryScheduled  = errors.New("no automatic provider retry is scheduled")
)
