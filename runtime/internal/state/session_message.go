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

// lateMessageRunner is the runner's memory of an acknowledgment whose caller had
// already given up. Older runners simply do not have one.
type lateMessageRunner interface {
	LateMessageResult(string) (proto.MessageResult, bool)
}

// LateMessageResult reports what this session's runner said about an operation
// after the request that started it was interrupted. It never speaks for the
// runner: an operation the runner has not answered has no result here.
func (s *Session) LateMessageResult(operationID string) (proto.MessageResult, bool) {
	s.mu.RLock()
	runner, supported := s.runner.(lateMessageRunner)
	s.mu.RUnlock()
	if !supported || operationID == "" {
		return proto.MessageResult{}, false
	}
	return runner.LateMessageResult(operationID)
}
