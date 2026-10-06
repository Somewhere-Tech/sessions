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
	Boundary    string `json:"boundary,omitempty"` // runner, queue or provider; not turn completion
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
	defer r.finishMessageWaiter(control.OperationID, response)
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

// finishMessageWaiter retires a waiter and its channel together, under the one
// lock that the reader also takes. Removing the waiter on its own left a race
// the size of a scheduling gap: the reader could see the waiter, queue the
// answer, and lose it to a cleanup that had already decided to give up. Both
// give-up paths -- caller cancellation and the acknowledgment timeout -- end
// here, so an answer that made it into the channel is kept as late evidence
// even when nobody read it. A caller that consumed its result leaves the
// channel empty and nothing is retained twice.
func (r *SocketRunner) finishMessageWaiter(operationID string, response chan MessageResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.messages, operationID)
	select {
	case result, open := <-response:
		if open {
			r.retainLateResultLocked(result)
		}
	default:
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
		return
	}
	// Nobody is waiting: the caller was cancelled or timed out while the runner
	// was already committing the message. The answer is still the runner's own
	// correlated statement about that operation, so this connection keeps it
	// instead of leaving a delivered message permanently unknown.
	r.retainLateResultLocked(result)
}

// lateMessageResultLimit bounds what an abandoned operation can cost this
// connection. Results are a few small fields each and only unclaimed ones are
// kept, so a client that keeps disconnecting cannot grow this without bound.
const lateMessageResultLimit = 64

func (r *SocketRunner) retainLateResultLocked(result MessageResult) {
	if result.OperationID == "" {
		return
	}
	if r.lateMessages == nil {
		r.lateMessages = make(map[string]MessageResult, 4)
	}
	if _, known := r.lateMessages[result.OperationID]; !known {
		if len(r.lateOrder) >= lateMessageResultLimit {
			delete(r.lateMessages, r.lateOrder[0])
			r.lateOrder = append(r.lateOrder[:0], r.lateOrder[1:]...)
		}
		r.lateOrder = append(r.lateOrder, result.OperationID)
	}
	r.lateMessages[result.OperationID] = result
}

// LateMessageResult returns an acknowledgment the runner sent for an operation
// whose caller had already stopped waiting. The memory is this connection's,
// here in the daemon, not the runner process's: replacing the connection or
// restarting the daemon loses it. That costs only the chance to settle a receipt
// that is still unknown; a receipt already confirmed from this evidence is
// durable and stays accepted. Nothing is invented -- an operation this
// connection never saw answered has no evidence.
func (r *SocketRunner) LateMessageResult(operationID string) (MessageResult, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result, ok := r.lateMessages[operationID]
	return result, ok
}
