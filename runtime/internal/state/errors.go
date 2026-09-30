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
	// ErrNoTerminalMirror: this session kind is not backed by a terminal
	// screen, so there is no rendered viewport to return. A lane answers from
	// its raw output tail and a structured provider from its event log; asking
	// either for a screen is a question with no answer, and an empty screen
	// would be a wrong one.
	ErrNoTerminalMirror = errors.New("this session kind has no terminal mirror")
)
