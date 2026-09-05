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
