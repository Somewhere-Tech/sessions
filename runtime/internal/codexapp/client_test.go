package codexapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestConversationTurnStreamsStructuredEventsAndAutoApproves(t *testing.T) {
	serverInput, clientInput := io.Pipe()
	clientOutput, serverOutput := io.Pipe()
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serveTestTurn(serverInput, serverOutput)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := newClient(ctx, clientInput, clientOutput, func() {
		_ = clientInput.Close()
		_ = clientOutput.Close()
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	conversationID, err := client.NewConversation(ctx, ConversationOptions{
		CWD:         "/tmp",
		Effort:      "high",
		ServiceTier: "priority",
	})
	if err != nil {
		t.Fatal(err)
	}
	if conversationID != "thread-1" {
		t.Fatalf("conversation id = %q", conversationID)
	}

	stream, err := client.SendUserTurn(ctx, conversationID, "Reply with exactly APPSERVER_OK.")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.InterruptTurn(ctx, conversationID); err != nil {
		t.Fatal(err)
	}
	// Deliberately await the result before draining Events. The event queue must
	// not deadlock or drop the pre-response notifications sent by the fake.
	result, err := stream.Result(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Message != "APPSERVER_OK" {
		t.Fatalf("message = %q", result.Message)
	}
	if result.TokenUsage.Last.TotalTokens != 31 {
		t.Fatalf("last token count = %d", result.TokenUsage.Last.TotalTokens)
	}
	if result.Status != "completed" {
		t.Fatalf("status = %q", result.Status)
	}

	var eventTypes []string
	var deltas strings.Builder
	for event := range stream.Events {
		eventTypes = append(eventTypes, fmt.Sprintf("%T", event))
		if delta, ok := event.(AgentMessageDelta); ok {
			deltas.WriteString(delta.Delta)
		}
	}
	if deltas.String() != "APPSERVER_OK" {
		t.Fatalf("agent-message deltas = %q", deltas.String())
	}
	wantTypes := []string{
		"codexapp.TurnStarted",
		"codexapp.ItemStarted",
		"codexapp.AgentMessageDelta",
		"codexapp.PlanUpdated",
		"codexapp.AgentMessageDelta",
		"codexapp.ItemCompleted",
		"codexapp.TokenCount",
		"codexapp.TurnComplete",
	}
	if strings.Join(eventTypes, ",") != strings.Join(wantTypes, ",") {
		t.Fatalf("event types = %v, want %v", eventTypes, wantTypes)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestSteerTurnUsesActiveTurnPrecondition(t *testing.T) {
	serverInput, clientInput := io.Pipe()
	clientOutput, serverOutput := io.Pipe()
	serverDone := make(chan error, 1)
	go func() {
		defer serverOutput.Close()
		decoder := json.NewDecoder(serverInput)
		encoder := json.NewEncoder(serverOutput)
		request, err := readTestRequest(decoder, "initialize")
		if err != nil {
			serverDone <- err
			return
		}
		if err := encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{}}); err != nil {
			serverDone <- err
			return
		}
		if _, err := readTestRequest(decoder, "initialized"); err != nil {
			serverDone <- err
			return
		}
		request, err = readTestRequest(decoder, "thread/start")
		if err != nil {
			serverDone <- err
			return
		}
		if err := encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{
			"thread": map[string]any{"id": "thread-1"},
		}}); err != nil {
			serverDone <- err
			return
		}
		request, err = readTestRequest(decoder, "turn/start")
		if err != nil {
			serverDone <- err
			return
		}
		if err := encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{
			"turn": map[string]any{"id": "turn-1", "items": []any{}, "status": "inProgress"},
		}}); err != nil {
			serverDone <- err
			return
		}
		request, err = readTestRequest(decoder, "turn/steer")
		if err != nil {
			serverDone <- err
			return
		}
		var steer TurnSteerParams
		if err := json.Unmarshal(request.Params, &steer); err != nil {
			serverDone <- err
			return
		}
		if steer.ThreadID != "thread-1" || steer.ExpectedTurnID != "turn-1" ||
			len(steer.Input) != 1 || steer.Input[0].Text != "also verify the tests" {
			serverDone <- fmt.Errorf("steer params = %+v", steer)
			return
		}
		if err := encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{"turnId": "turn-1"}}); err != nil {
			serverDone <- err
			return
		}
		serverDone <- encoder.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{
			"threadId": "thread-1", "turn": map[string]any{"id": "turn-1", "items": []any{}, "status": "completed"},
		}})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := newClient(ctx, clientInput, clientOutput, func() {
		_ = clientInput.Close()
		_ = clientOutput.Close()
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	conversationID, err := client.NewConversation(ctx, ConversationOptions{CWD: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.SendUserTurn(ctx, conversationID, "start")
	if err != nil {
		t.Fatal(err)
	}
	turnID, err := client.SteerTurn(ctx, conversationID, "also verify the tests")
	if err != nil {
		t.Fatal(err)
	}
	if turnID != "turn-1" {
		t.Fatalf("steered turn id = %q", turnID)
	}
	if _, err := stream.Result(ctx); err != nil {
		t.Fatal(err)
	}
	for range stream.Events {
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestNotificationWithoutTurnIDDoesNotMutateActiveTurn(t *testing.T) {
	state := newTurnState("thread-1")
	if !state.acceptTurnID("turn-1") {
		t.Fatal("failed to initialize active turn id")
	}
	client := &Client{turns: map[string]*turnState{"thread-1": state}}

	client.handleNotification("item/agentMessage/delta", json.RawMessage(
		`{"threadId":"thread-1","itemId":"agent-1","delta":"untrusted"}`,
	))

	state.mu.Lock()
	defer state.mu.Unlock()
	if got := state.deltas["agent-1"]; got != "" {
		t.Fatalf("notification without turn id mutated active turn: %q", got)
	}
}

func TestInjectMessagesUsesAuthoredResponseItemsOnly(t *testing.T) {
	serverInput, clientInput := io.Pipe()
	clientOutput, serverOutput := io.Pipe()
	serverDone := make(chan error, 1)
	go func() {
		defer serverOutput.Close()
		decoder := json.NewDecoder(serverInput)
		encoder := json.NewEncoder(serverOutput)
		request, err := readTestRequest(decoder, "initialize")
		if err != nil {
			serverDone <- err
			return
		}
		if err := encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{}}); err != nil {
			serverDone <- err
			return
		}
		if _, err := readTestRequest(decoder, "initialized"); err != nil {
			serverDone <- err
			return
		}
		request, err = readTestRequest(decoder, "thread/inject_items")
		if err != nil {
			serverDone <- err
			return
		}
		var params struct {
			ThreadID string `json:"threadId"`
			Items    []struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"items"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			serverDone <- err
			return
		}
		if params.ThreadID != "thread-1" || len(params.Items) != 2 ||
			params.Items[0].Role != "user" || params.Items[0].Content[0].Type != "input_text" ||
			params.Items[1].Role != "assistant" || params.Items[1].Content[0].Type != "output_text" {
			serverDone <- fmt.Errorf("inject params = %+v", params)
			return
		}
		serverDone <- encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{}})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := newClient(ctx, clientInput, clientOutput, func() {
		_ = clientInput.Close()
		_ = clientOutput.Close()
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.InjectMessages(ctx, "thread-1", []ImportedMessage{
		{Role: "user", Text: "Question with `code`."},
		{Role: "assistant", Text: "Answer."},
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestDuplicateAndOutOfOrderNotificationsKeepCompletedResultStable(t *testing.T) {
	state := newTurnState("thread-1")
	if !state.acceptTurnID("turn-1") {
		t.Fatal("failed to initialize active turn id")
	}
	client := &Client{turns: map[string]*turnState{"thread-1": state}}
	stream := state.stream()
	consumed := make(chan struct{})
	go func() {
		for range stream.Events {
		}
		close(consumed)
	}()

	completedItem := json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","item":{"id":"agent-1","type":"agentMessage","text":"authoritative"}}`)
	client.handleNotification("item/agentMessage/delta", json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"agent-1","delta":"partial"}`))
	client.handleNotification("item/completed", completedItem)
	client.handleNotification("item/completed", completedItem)
	client.handleNotification("turn/completed", json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed","items":[{"id":"agent-1","type":"agentMessage","text":"authoritative"}]}}`))

	// Late notifications and duplicate completion are ignored after the turn is
	// removed; they cannot reopen or mutate the completed result.
	client.handleNotification("item/agentMessage/delta", json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"agent-1","delta":"poison"}`))
	client.handleNotification("turn/completed", json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"failed","items":[]}}`))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := stream.Result(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Message != "authoritative" || result.Status != "completed" {
		t.Fatalf("completed result was poisoned: %#v", result)
	}
	select {
	case <-consumed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestReadLoopSkipsMalformedFrameAndProcessesFollowingEvent(t *testing.T) {
	state := newTurnState("thread-1")
	if !state.acceptTurnID("turn-1") {
		t.Fatal("failed to initialize active turn id")
	}
	client := &Client{
		transport: &scriptedMessageTransport{messages: [][]byte{
			[]byte(`{"method":"truncated"`),
			[]byte(`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed","items":[]}}}`),
		}},
		pending: make(map[string]chan callResponse),
		turns:   map[string]*turnState{"thread-1": state},
	}
	stream := state.stream()

	client.readLoop(false)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := stream.Result(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" {
		t.Fatalf("following valid event was not processed: %#v", result)
	}
	for range stream.Events {
	}
}

type scriptedMessageTransport struct {
	messages [][]byte
	next     int
}

func (t *scriptedMessageTransport) Read(context.Context) ([]byte, error) {
	if t.next == len(t.messages) {
		return nil, io.EOF
	}
	message := t.messages[t.next]
	t.next++
	return message, nil
}

func (*scriptedMessageTransport) Write(context.Context, []byte) error { return nil }
func (*scriptedMessageTransport) Close() error                        { return nil }

func TestResumeConversationRestoresTurnDefaults(t *testing.T) {
	serverInput, clientInput := io.Pipe()
	clientOutput, serverOutput := io.Pipe()
	serverDone := make(chan error, 1)
	go func() {
		defer serverOutput.Close()
		decoder := json.NewDecoder(serverInput)
		encode := json.NewEncoder(serverOutput)
		request, err := readTestRequest(decoder, "initialize")
		if err != nil {
			serverDone <- err
			return
		}
		if err := encode.Encode(map[string]any{"id": request.ID, "result": map[string]any{}}); err != nil {
			serverDone <- err
			return
		}
		if _, err := readTestRequest(decoder, "initialized"); err != nil {
			serverDone <- err
			return
		}
		request, err = readTestRequest(decoder, "thread/resume")
		if err != nil {
			serverDone <- err
			return
		}
		var resume threadResumeParams
		if err := json.Unmarshal(request.Params, &resume); err != nil {
			serverDone <- err
			return
		}
		if resume.ThreadID != "thread-1" || resume.Model != "test-model" || resume.ApprovalPolicy != ApprovalNever ||
			resume.ServiceTier != "priority" || resume.Config["model_reasoning_effort"] != "high" ||
			resume.DeveloperInstructions != "preserve imported context" {
			serverDone <- fmt.Errorf("resume params = %+v", resume)
			return
		}
		if err := encode.Encode(map[string]any{"id": request.ID, "result": map[string]any{
			"thread": map[string]any{"id": "thread-1"},
		}}); err != nil {
			serverDone <- err
			return
		}
		request, err = readTestRequest(decoder, "turn/start")
		if err != nil {
			serverDone <- err
			return
		}
		var turn TurnStartParams
		if err := json.Unmarshal(request.Params, &turn); err != nil {
			serverDone <- err
			return
		}
		if turn.ThreadID != "thread-1" || turn.Model != "test-model" || turn.Effort != "high" || turn.ServiceTier != "priority" {
			serverDone <- fmt.Errorf("resumed turn params = %+v", turn)
			return
		}
		if err := encode.Encode(map[string]any{"id": request.ID, "result": map[string]any{
			"turn": map[string]any{"id": "turn-1", "items": []any{}, "status": "inProgress"},
		}}); err != nil {
			serverDone <- err
			return
		}
		serverDone <- encode.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{
			"threadId": "thread-1", "turn": map[string]any{"id": "turn-1", "items": []any{}, "status": "completed"},
		}})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := newClient(ctx, clientInput, clientOutput, func() {
		_ = clientInput.Close()
		_ = clientOutput.Close()
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	conversation, err := client.ResumeConversation(ctx, "thread-1", ConversationOptions{
		CWD: "/tmp", Model: "test-model", Effort: "high", ServiceTier: "priority",
		DeveloperInstructions: "preserve imported context",
	})
	if err != nil {
		t.Fatal(err)
	}
	if conversation.ID != "thread-1" {
		t.Fatalf("resumed conversation = %#v", conversation)
	}
	stream, err := client.SendUserTurn(ctx, conversation.ID, "continue")
	if err != nil {
		t.Fatal(err)
	}
	if result, err := stream.Result(ctx); err != nil || result.Status != "completed" {
		t.Fatalf("resumed turn result = %#v, %v", result, err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func serveTestTurn(reader io.Reader, writer io.WriteCloser) error {
	defer writer.Close()
	decoder := json.NewDecoder(reader)
	encode := json.NewEncoder(writer)

	request, err := readTestRequest(decoder, "initialize")
	if err != nil {
		return err
	}
	if err := encode.Encode(map[string]any{
		"id": request.ID,
		"result": map[string]any{
			"codexHome":      "/tmp/codex-home",
			"platformFamily": "unix",
			"platformOs":     "macos",
			"userAgent":      "codex/0.144.5",
		},
	}); err != nil {
		return err
	}
	initialized, err := readTestRequest(decoder, "initialized")
	if err != nil {
		return err
	}
	if len(initialized.ID) != 0 {
		return errors.New("initialized unexpectedly had an id")
	}

	request, err = readTestRequest(decoder, "thread/start")
	if err != nil {
		return err
	}
	var threadParams ThreadStartParams
	if err := json.Unmarshal(request.Params, &threadParams); err != nil {
		return err
	}
	if threadParams.CWD != "/tmp" || threadParams.ApprovalPolicy != ApprovalNever || threadParams.Sandbox != SandboxDangerFullAccess ||
		threadParams.ServiceTier != "priority" || threadParams.Config["model_reasoning_effort"] != "high" {
		return fmt.Errorf("unexpected thread/start params: %+v", threadParams)
	}
	if err := encode.Encode(map[string]any{
		"id": request.ID,
		"result": map[string]any{
			"approvalPolicy": ApprovalNever,
			"cwd":            "/tmp",
			"model":          "test-model",
			"modelProvider":  "test",
			"thread":         map[string]any{"id": "thread-1", "sessionId": "thread-1", "cwd": "/tmp"},
		},
	}); err != nil {
		return err
	}

	request, err = readTestRequest(decoder, "turn/start")
	if err != nil {
		return err
	}
	var turnParams TurnStartParams
	if err := json.Unmarshal(request.Params, &turnParams); err != nil {
		return err
	}
	if turnParams.ThreadID != "thread-1" || turnParams.Effort != "high" || turnParams.ApprovalPolicy != ApprovalNever ||
		turnParams.ServiceTier != "priority" {
		return fmt.Errorf("unexpected turn/start params: %+v", turnParams)
	}
	if len(turnParams.Input) != 1 || turnParams.Input[0].Text != "Reply with exactly APPSERVER_OK." {
		return fmt.Errorf("unexpected turn input: %+v", turnParams.Input)
	}

	// A server request can arrive while turn/start is outstanding. The client
	// must correlate it independently and opt into the session approval cache.
	if err := encode.Encode(map[string]any{
		"id":     "approval-1",
		"method": "item/commandExecution/requestApproval",
		"params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "itemId": "command-1", "startedAtMs": 1},
	}); err != nil {
		return err
	}
	approval, err := readTestResponse(decoder)
	if err != nil {
		return err
	}
	if string(approval.ID) != `"approval-1"` {
		return fmt.Errorf("approval response id = %s", approval.ID)
	}
	var approvalResult struct {
		Decision string `json:"decision"`
	}
	if err := json.Unmarshal(approval.Result, &approvalResult); err != nil {
		return err
	}
	if approvalResult.Decision != "acceptForSession" {
		return fmt.Errorf("approval decision = %q", approvalResult.Decision)
	}

	// Exercise the valid race where streaming starts before turn/start's
	// response reaches the client.
	for _, message := range []map[string]any{
		{
			"method": "item/started",
			"params": map[string]any{
				"threadId": "thread-1", "turnId": "turn-1", "startedAtMs": 10,
				"item": map[string]any{"id": "agent-1", "type": "agentMessage", "text": ""},
			},
		},
		{
			"method": "item/agentMessage/delta",
			"params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "itemId": "agent-1", "delta": "APPSERVER_"},
		},
	} {
		if err := encode.Encode(message); err != nil {
			return err
		}
	}
	if err := encode.Encode(map[string]any{
		"id": request.ID,
		"result": map[string]any{
			"turn": map[string]any{"id": "turn-1", "items": []any{}, "status": "inProgress"},
		},
	}); err != nil {
		return err
	}
	interrupt, err := readTestRequest(decoder, "turn/interrupt")
	if err != nil {
		return err
	}
	var interruptParams struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
	}
	if err := json.Unmarshal(interrupt.Params, &interruptParams); err != nil {
		return err
	}
	if interruptParams.ThreadID != "thread-1" || interruptParams.TurnID != "turn-1" {
		return fmt.Errorf("unexpected turn/interrupt params: %+v", interruptParams)
	}
	if err := encode.Encode(map[string]any{"id": interrupt.ID, "result": map[string]any{}}); err != nil {
		return err
	}

	finalPhase := "final"
	for _, message := range []map[string]any{
		{
			"method": "turn/plan/updated",
			"params": map[string]any{
				"threadId": "thread-1", "turnId": "turn-1", "explanation": "Ship the GUI",
				"plan": []any{
					map[string]any{"step": "Inspect", "status": "completed"},
					map[string]any{"step": "Build", "status": "inProgress"},
				},
			},
		},
		{
			"method": "item/agentMessage/delta",
			"params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "itemId": "agent-1", "delta": "OK"},
		},
		{
			"method": "item/completed",
			"params": map[string]any{
				"threadId": "thread-1", "turnId": "turn-1", "completedAtMs": 20,
				"item": map[string]any{"id": "agent-1", "type": "agentMessage", "text": "APPSERVER_OK", "phase": finalPhase},
			},
		},
		{
			"method": "thread/tokenUsage/updated",
			"params": map[string]any{
				"threadId": "thread-1", "turnId": "turn-1",
				"tokenUsage": map[string]any{
					"last":  map[string]any{"cachedInputTokens": 2, "inputTokens": 20, "outputTokens": 11, "reasoningOutputTokens": 0, "totalTokens": 31},
					"total": map[string]any{"cachedInputTokens": 2, "inputTokens": 20, "outputTokens": 11, "reasoningOutputTokens": 0, "totalTokens": 31},
				},
			},
		},
		{
			"method": "turn/completed",
			"params": map[string]any{
				"threadId": "thread-1",
				"turn": map[string]any{
					"id": "turn-1", "status": "completed",
					"items": []any{map[string]any{"id": "agent-1", "type": "agentMessage", "text": "APPSERVER_OK", "phase": finalPhase}},
				},
			},
		},
	} {
		if err := encode.Encode(message); err != nil {
			return err
		}
	}
	return nil
}

type testMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
}

func readTestRequest(decoder *json.Decoder, method string) (testMessage, error) {
	var message testMessage
	if err := decoder.Decode(&message); err != nil {
		return testMessage{}, err
	}
	if message.Method != method {
		return testMessage{}, fmt.Errorf("method = %q, want %q", message.Method, method)
	}
	return message, nil
}

func readTestResponse(decoder *json.Decoder) (testMessage, error) {
	var message testMessage
	if err := decoder.Decode(&message); err != nil {
		return testMessage{}, err
	}
	if message.Method != "" || len(message.ID) == 0 || len(message.Result) == 0 {
		return testMessage{}, fmt.Errorf("not a response: %+v", message)
	}
	return message, nil
}
