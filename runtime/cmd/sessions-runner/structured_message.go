package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
)

func replyMessage(c *client, payload []byte, submit func(proto.MessageControl) proto.MessageResult) error {
	var control proto.MessageControl
	err := json.Unmarshal(payload, &control)
	if err == nil {
		err = proto.ValidateMessage(control)
	}
	result := proto.MessageResult{OperationID: control.OperationID}
	if err != nil {
		result.Error = err.Error()
	} else {
		result = submit(control)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return c.write(proto.MessageRes, encoded)
}

func (r *codexAppRunner) submitMessage(control proto.MessageControl) proto.MessageResult {
	result := proto.MessageResult{OperationID: control.OperationID}
	r.mu.Lock()
	active := r.active
	if !active && control.Mode == "steer" {
		r.mu.Unlock()
		result.Error = "Codex has no active turn to steer. Send this as a new message."
		return result
	}
	if !active {
		r.active = true
	}
	r.mu.Unlock()
	if !active {
		if r.retry != nil {
			r.retry.Replace()
		}
		go r.runTurn(control.Text, 0, true)
		result.Accepted, result.Boundary = true, "runner"
		return result
	}
	r.steerMu.Lock()
	defer r.steerMu.Unlock()
	submittedAt := time.Now()
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	turnID, err := r.turnClient.SteerTurn(ctx, r.conversationID, control.Text)
	if errors.Is(err, codexapp.ErrSteerRefused) {
		// The turn ended before the request was written. A known refusal.
		result.Error = "Codex finished its turn before this message could be sent: " + err.Error() + "."
		return result
	}
	if err != nil {
		result.Error = "Codex did not confirm steering: " + err.Error()
		// Once written, any failure (an error answer included) is ambiguous:
		// Codex may already have queued the input. Never a safe-to-retry refusal.
		result.Boundary = "unknown"
		return result
	}
	if event, err := codexapp.SteeringHistoryEvent(r.conversationID, turnID, control.Text, submittedAt); err == nil {
		r.appendStructured(event)
	}
	result.Accepted, result.Boundary = true, "provider"
	return result
}

func (r *claudeStructuredRunner) submitMessage(control proto.MessageControl) proto.MessageResult {
	result := proto.MessageResult{OperationID: control.OperationID}
	r.mu.Lock()
	if r.active || control.Mode == "steer" {
		r.mu.Unlock()
		result.Error = "Claude cannot accept this message during an active turn. Your draft was not sent."
		return result
	}
	r.active = true
	r.mu.Unlock()
	if r.retry != nil {
		r.retry.Replace()
	}
	go r.runTurn(control.Text, 0, true)
	result.Accepted, result.Boundary = true, "runner"
	return result
}
