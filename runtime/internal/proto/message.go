package proto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

type MessageControl struct {
	OperationID string `json:"operation_id"`
	Text        string `json:"text"`
	Mode        string `json:"mode,omitempty"` // auto or steer; never terminal control bytes
}

type MessageResult struct {
	OperationID string `json:"operation_id"`
	Accepted    bool   `json:"accepted"`
	Boundary    string `json:"boundary,omitempty"` // runner or provider, not turn completion
	Error       string `json:"error,omitempty"`
}

type MessageRunner interface {
	SubmitMessage(context.Context, MessageControl) (MessageResult, error)
}

func ValidateMessage(control MessageControl) error {
	if control.OperationID == "" || len(control.OperationID) > 128 {
		return errors.New("message requires a bounded operation id")
	}
	if strings.TrimSpace(control.Text) == "" {
		return errors.New("message text is empty")
	}
	if control.Mode != "" && control.Mode != "auto" && control.Mode != "steer" {
		return errors.New("message mode must be auto or steer")
	}
	return nil
}

func (r *SocketRunner) SubmitMessage(ctx context.Context, control MessageControl) (MessageResult, error) {
	if err := ctx.Err(); err != nil {
		return MessageResult{}, err
	}
	if !r.Info().MessageSubmit {
		return MessageResult{}, errors.New("this runner has no acknowledged message control")
	}
	if err := ValidateMessage(control); err != nil {
		return MessageResult{}, err
	}
	payload, err := json.Marshal(control)
	if err != nil {
		return MessageResult{}, err
	}
	r.startReader()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return MessageResult{}, net.ErrClosed
	}
	if r.messages == nil {
		r.messages = make(map[string]chan MessageResult)
	}
	if _, exists := r.messages[control.OperationID]; exists {
		r.mu.Unlock()
		return MessageResult{}, errors.New("message operation is already in flight")
	}
	response := make(chan MessageResult, 1)
	r.messages[control.OperationID] = response
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.messages, control.OperationID); r.mu.Unlock() }()
	if err := r.write(MessageReq, payload); err != nil {
		return MessageResult{}, err
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case result, open := <-response:
		if !open {
			return MessageResult{}, net.ErrClosed
		}
		return result, nil
	case <-ctx.Done():
		return MessageResult{}, ctx.Err()
	case <-timer.C:
		return MessageResult{}, fmt.Errorf("message %s acknowledgment timed out; delivery is unknown, do not resend automatically", control.OperationID)
	}
}

func (r *SocketRunner) handleMessageResponse(payload []byte) {
	var result MessageResult
	if json.Unmarshal(payload, &result) != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if response := r.messages[result.OperationID]; response != nil {
		select {
		case response <- result:
		default:
		}
	}
}
