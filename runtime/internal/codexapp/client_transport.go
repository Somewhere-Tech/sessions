package codexapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

func validApprovalPolicy(policy string) bool {
	return policy == ApprovalUntrusted || policy == ApprovalOnRequest || policy == ApprovalNever
}

func validSandbox(sandbox string) bool {
	return sandbox == SandboxReadOnly || sandbox == SandboxWorkspaceWrite || sandbox == SandboxDangerFullAccess
}

func (c *Client) call(ctx context.Context, method string, params, output any) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.nextID++
	id := c.nextID
	key := strconv.FormatUint(id, 10)
	response := make(chan callResponse, 1)
	c.pending[key] = response
	c.mu.Unlock()

	request := struct {
		ID     uint64 `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{ID: id, Method: method, Params: params}
	if err := c.writeJSON(request); err != nil {
		c.removePending(key)
		return err
	}

	// The per-call response is the single terminal signal: handleResponse
	// supplies a result, while fail resolves every still-pending call with an
	// error. Do not also select on client closure here. A valid reply may already
	// be buffered when the read loop observes a following EOF, and competing
	// ready channels would randomly discard that reply.
	select {
	case reply := <-response:
		if reply.err != nil {
			return reply.err
		}
		if output == nil {
			return nil
		}
		if err := json.Unmarshal(reply.result, output); err != nil {
			return fmt.Errorf("decode %s response: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		c.removePending(key)
		return ctx.Err()
	}
}

func (c *Client) removePending(key string) {
	c.mu.Lock()
	delete(c.pending, key)
	c.mu.Unlock()
}

func (c *Client) removeTurn(conversationID string, state *turnState) {
	c.mu.Lock()
	if c.turns[conversationID] == state {
		delete(c.turns, conversationID)
	}
	c.mu.Unlock()
}

func (c *Client) writeJSON(value any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrClosed
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode JSON-RPC message: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	if err := c.transport.Write(ctx, data); err != nil {
		c.fail(err)
		return fmt.Errorf("write JSON-RPC message: %w", err)
	}
	return nil
}

func (c *Client) readLoop(waitForProcess bool) {
	for {
		data, err := c.transport.Read(context.Background())
		if err != nil {
			if waitForProcess && errors.Is(err, io.EOF) {
				return
			}
			if !errors.Is(err, io.EOF) {
				c.fail(fmt.Errorf("read JSON-RPC message: %w", err))
			} else {
				c.fail(io.EOF)
			}
			return
		}
		message, err := decodeJSONRPC(data)
		if err != nil {
			// A malformed external frame cannot be correlated safely. Ignore it
			// without taking down unrelated pending calls or active turns.
			continue
		}
		if message.Method != "" && len(message.ID) > 0 {
			c.handleServerRequest(message)
			continue
		}
		if message.Method != "" {
			c.handleNotification(message.Method, message.Params)
			continue
		}
		if len(message.ID) > 0 {
			c.handleResponse(message)
		}
	}
}

func (c *Client) watchProcess() {
	result, ok := <-c.process
	if !ok {
		return
	}
	if result.err == nil {
		c.fail(io.EOF)
		return
	}
	if result.stderr == "" {
		c.fail(fmt.Errorf("codex app-server proxy exited: %w", result.err))
		return
	}
	c.fail(fmt.Errorf("codex app-server proxy exited: %w: %s", result.err, result.stderr))
}

func (c *Client) handleResponse(message wireMessage) {
	key := strings.TrimSpace(string(message.ID))
	c.mu.Lock()
	pending := c.pending[key]
	delete(c.pending, key)
	c.mu.Unlock()
	if pending == nil {
		return
	}
	if message.Error != nil {
		pending <- callResponse{err: *message.Error}
		return
	}
	pending <- callResponse{result: message.Result}
}

func (c *Client) handleServerRequest(message wireMessage) {
	if !isApprovalMethod(message.Method) {
		_ = c.writeJSON(struct {
			ID    json.RawMessage `json:"id"`
			Error rpcError        `json:"error"`
		}{
			ID: message.ID,
			Error: rpcError{
				Code:    -32601,
				Message: "unsupported server request: " + message.Method,
			},
		})
		return
	}
	request := parseApprovalRequest(message.Method, message.Params)
	c.mu.Lock()
	handler := c.approvals
	c.mu.Unlock()
	reply := func(decision ApprovalDecision) {
		_ = c.writeJSON(struct {
			ID     json.RawMessage `json:"id"`
			Result any             `json:"result"`
		}{ID: message.ID, Result: approvalReply(message.Method, request, decision)})
	}
	if handler == nil {
		reply(ApprovalAllowForSession)
		return
	}
	// The decision can take as long as a person takes; it must not hold the
	// read loop, which still has to deliver the turn's other events.
	go reply(handler(context.Background(), request))
}

func (c *Client) handleNotification(method string, params json.RawMessage) {
	parsed, err := parseServerEvent(method, params)
	if err != nil {
		return
	}
	switch event := parsed.event.(type) {
	case AgentMessageDelta:
		state := c.turn(event.ConversationID, event.TurnID)
		if state != nil {
			state.emit(event)
		}
	case ItemStarted:
		state := c.turn(event.ConversationID, event.TurnID)
		if state != nil {
			state.emit(event)
		}
	case ItemCompleted:
		state := c.turn(event.ConversationID, event.TurnID)
		if state != nil {
			state.emit(event)
		}
	case PlanUpdated:
		state := c.turn(event.ConversationID, event.TurnID)
		if state != nil {
			state.emit(event)
		}
	case TokenCount:
		state := c.turn(event.ConversationID, event.TurnID)
		if state != nil {
			state.emit(event)
		}
	case TurnComplete:
		state := c.turn(event.ConversationID, event.TurnID)
		if state != nil {
			state.complete(event, parsed.items)
			c.removeTurn(event.ConversationID, state)
		}
	}
}

func (c *Client) turn(conversationID, turnID string) *turnState {
	c.mu.Lock()
	state := c.turns[conversationID]
	c.mu.Unlock()
	if state == nil || !state.acceptTurnID(turnID) {
		return nil
	}
	return state
}

func (c *Client) fail(err error) {
	if err == nil {
		err = ErrClosed
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	pending := c.pending
	turns := c.turns
	c.pending = make(map[string]chan callResponse)
	c.turns = make(map[string]*turnState)
	c.mu.Unlock()

	for _, response := range pending {
		response <- callResponse{err: err}
	}
	for _, state := range turns {
		state.fail(err)
	}
}
