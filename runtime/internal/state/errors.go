package state

import "errors"

var (
	ErrSessionNotFound = errors.New("session not found")
	ErrSessionEnded    = errors.New("session has ended")
	// ErrNoPendingApproval: nothing is waiting for a decision on this session.
	ErrNoPendingApproval = errors.New("nothing is waiting for approval")
	ErrSessionWorking    = errors.New("session is working")
	ErrRunnerProtocol    = errors.New("runner protocol does not support this operation")
)
