package state

import "errors"

var (
	ErrSessionNotFound = errors.New("session not found")
	ErrSessionEnded    = errors.New("session has ended")
	ErrSessionWorking  = errors.New("session is working")
	ErrRunnerProtocol  = errors.New("runner protocol does not support this operation")
)
