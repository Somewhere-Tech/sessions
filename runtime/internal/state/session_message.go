package state

import (
	"context"
	"errors"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
)

func (s *Session) SubmitMessage(ctx context.Context, control proto.MessageControl) (proto.MessageResult, error) {
	s.mu.RLock()
	runner, supported := s.runner.(proto.MessageRunner)
	available := s.info.MessageSubmit && !s.info.Exited && !s.info.Unreachable
	s.mu.RUnlock()
	if !supported || !available {
		return proto.MessageResult{}, errors.New("session does not support acknowledged messages right now")
	}
	return runner.SubmitMessage(ctx, control)
}

// lateMessageRunner is a runner client that remembers an acknowledgment whose
// caller had already given up. It is an implementation detail of the daemon's
// side of the connection, not a runner capability: a client type that does not
// keep one simply has nothing to report, and no runner binary is asked for
// anything new.
type lateMessageRunner interface {
	LateMessageResult(string) (proto.MessageResult, bool)
}

// LateMessageResult reports what the runner said about an operation after the
// request that started it was interrupted, as far as this daemon's live
// connection to it still remembers. It never speaks for the runner: an
// unanswered operation, a replaced connection, and a restarted daemon all
// return nothing rather than a guess.
func (s *Session) LateMessageResult(operationID string) (proto.MessageResult, bool) {
	s.mu.RLock()
	runner, supported := s.runner.(lateMessageRunner)
	s.mu.RUnlock()
	if !supported || operationID == "" {
		return proto.MessageResult{}, false
	}
	return runner.LateMessageResult(operationID)
}
