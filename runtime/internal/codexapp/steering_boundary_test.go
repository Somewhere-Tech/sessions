package codexapp

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"
)

type steeringBoundaryTransport struct{ write func([]byte) error }

func (s steeringBoundaryTransport) Read(context.Context) ([]byte, error)       { return nil, io.EOF }
func (s steeringBoundaryTransport) Write(_ context.Context, data []byte) error { return s.write(data) }
func (s steeringBoundaryTransport) Close() error                               { return nil }

// The provider is a protocol fixture, not an authenticated model. It holds a
// tool boundary and applies only input received through the active-turn API.
func TestSteeringToolBoundaryAndCompletionBeforeReply(t *testing.T) {
	for _, completionFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "tool-still-running", true: "completion-before-reply"}[completionFirst], func(t *testing.T) {
			state := newTurnState("thread-1")
			state.acceptTurnID("turn-1")
			client := &Client{
				turns:   map[string]*turnState{"thread-1": state},
				pending: make(map[string]chan callResponse),
			}
			var input string
			writes := 0
			completeTool := func() {
				params, err := json.Marshal(map[string]any{
					"threadId": "thread-1", "turn": map[string]any{
						"id": "turn-1", "status": "completed", "items": []any{
							map[string]any{"id": "answer-1", "type": "agentMessage", "text": input},
						},
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				client.handleNotification("turn/completed", params)
			}
			client.transport = steeringBoundaryTransport{write: func(data []byte) error {
				writes++
				request, err := decodeJSONRPC(data)
				if err != nil {
					return err
				}
				var params TurnSteerParams
				if err := json.Unmarshal(request.Params, &params); err != nil {
					return err
				}
				if request.Method != "turn/steer" || params.ThreadID != "thread-1" || params.ExpectedTurnID != "turn-1" || len(params.Input) != 1 {
					t.Fatalf("wrong steering request: %s", data)
				}
				input = params.Input[0].Text
				if completionFirst {
					completeTool()
				}
				client.handleResponse(wireMessage{ID: request.ID, Result: json.RawMessage(`{"turnId":"turn-1"}`)})
				return nil
			}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			turnID, err := client.SteerTurn(ctx, "thread-1", "STEER RECEIVED")
			if err != nil || turnID != "turn-1" {
				t.Fatalf("steer = %q, %v", turnID, err)
			}
			if !completionFirst {
				select {
				case <-state.done:
					t.Fatal("acceptance falsely completed the active tool")
				default:
				}
				completeTool()
			}
			stream := state.stream()
			result, err := stream.Result(ctx)
			if err != nil || result.TurnID != turnID || result.Message != "STEER RECEIVED" {
				t.Fatalf("same-turn result = %+v, %v", result, err)
			}
			for range stream.Events {
			}
			if _, err := client.SteerTurn(ctx, "thread-1", "do not replay into a new turn"); err == nil || writes != 1 {
				t.Fatalf("idle steer wrote another request: writes=%d err=%v", writes, err)
			}
		})
	}
}
