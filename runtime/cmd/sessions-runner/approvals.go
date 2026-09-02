package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
)

// approvalDesk holds the permission requests a structured provider is waiting
// on, keyed by the id announced on the structured stream, until the daemon
// answers each with an Approve frame. Both Rich runners share it: Codex asks
// through its app-server client, Claude through the permission-prompt shim.
type approvalDesk struct {
	mu      sync.Mutex
	pending map[string]chan proto.ApprovalControl
	seq     uint64
}

// open registers a new request and returns its id and the channel its answer
// arrives on.
func (d *approvalDesk) open() (string, chan proto.ApprovalControl) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seq++
	id := fmt.Sprintf("approval-%d", d.seq)
	decided := make(chan proto.ApprovalControl, 1)
	if d.pending == nil {
		d.pending = make(map[string]chan proto.ApprovalControl)
	}
	d.pending[id] = decided
	return id, decided
}

// wait blocks until the request is answered or either context ends. An
// unanswered request is denied, so a provider never proceeds on silence.
func (d *approvalDesk) wait(ctx, runner context.Context, id string, decided chan proto.ApprovalControl) proto.ApprovalControl {
	control := proto.ApprovalControl{ID: id, Decision: proto.ApprovalDeny}
	select {
	case control = <-decided:
	case <-ctx.Done():
	case <-runner.Done():
	}
	d.mu.Lock()
	delete(d.pending, id)
	d.mu.Unlock()
	return control
}

func (d *approvalDesk) resolve(control proto.ApprovalControl) error {
	d.mu.Lock()
	decided, ok := d.pending[control.ID]
	if ok {
		delete(d.pending, control.ID)
	}
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("no approval %q is waiting", control.ID)
	}
	decided <- control
	return nil
}

// shimApprovalRequest is what the permission-prompt shim sends the runner:
// one line of JSON per connection, answered with one shimApprovalReply line.
type shimApprovalRequest struct {
	ToolName  string          `json:"tool_name"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

type shimApprovalReply struct {
	Decision string `json:"decision"`
	By       string `json:"by,omitempty"`
}

// serveApprovalSocket answers shim connections until the listener closes.
// decide is called per request and may block for as long as a person takes.
func serveApprovalSocket(listener net.Listener, decide func(shimApprovalRequest) shimApprovalReply) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		go func(connection net.Conn) {
			defer connection.Close()
			line, err := bufio.NewReader(connection).ReadBytes('\n')
			if err != nil && len(line) == 0 {
				return
			}
			var request shimApprovalRequest
			if json.Unmarshal(line, &request) != nil {
				return
			}
			reply := decide(request)
			encoded, err := json.Marshal(reply)
			if err != nil {
				return
			}
			_, _ = connection.Write(append(encoded, '\n'))
		}(connection)
	}
}

// askRunnerApproval is the shim side: one request, one reply, over the
// runner's approval socket.
func askRunnerApproval(ctx context.Context, dial func(context.Context) (net.Conn, error), request shimApprovalRequest) (shimApprovalReply, error) {
	connection, err := dial(ctx)
	if err != nil {
		return shimApprovalReply{}, err
	}
	defer connection.Close()
	encoded, err := json.Marshal(request)
	if err != nil {
		return shimApprovalReply{}, err
	}
	if _, err := connection.Write(append(encoded, '\n')); err != nil {
		return shimApprovalReply{}, err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-done:
		}
	}()
	defer close(done)
	line, err := bufio.NewReader(connection).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		if ctx.Err() != nil {
			return shimApprovalReply{}, ctx.Err()
		}
		return shimApprovalReply{}, err
	}
	var reply shimApprovalReply
	if err := json.Unmarshal(line, &reply); err != nil {
		return shimApprovalReply{}, err
	}
	if !proto.ValidApprovalDecision(reply.Decision) {
		return shimApprovalReply{}, errors.New("runner answered with an unknown decision")
	}
	return reply, nil
}
