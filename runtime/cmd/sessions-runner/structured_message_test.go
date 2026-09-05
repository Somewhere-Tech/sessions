package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
)

type completingSteerClient struct {
	*fakeCodexTurnClient
	beforeReply func()
}

func (c completingSteerClient) SteerTurn(ctx context.Context, conversationID, text string) (string, error) {
	c.beforeReply()
	return c.fakeCodexTurnClient.SteerTurn(ctx, conversationID, text)
}

func TestSteeringHistoryKeepsSubmissionOrderWhenCompletionPrecedesAck(t *testing.T) {
	for _, typed := range []bool{true, false} {
		runner := newCodexTestRunner(t)
		runner.active = true
		var completedAt time.Time
		runner.turnClient = completingSteerClient{
			fakeCodexTurnClient: runner.turnClient.(*fakeCodexTurnClient),
			beforeReply: func() {
				completedAt = time.Now()
				event, err := codexapp.HistoryEvent(codexapp.TurnComplete{
					ConversationID: "thread-1", TurnID: "turn-1", Status: "completed",
				}, completedAt)
				if err != nil {
					t.Fatal(err)
				}
				runner.appendStructured(event)
			},
		}
		if typed {
			result := runner.submitMessage(proto.MessageControl{OperationID: "late-ack", Text: "Update the answer", Mode: "steer"})
			if !result.Accepted || result.Boundary != "provider" {
				t.Fatalf("result = %#v", result)
			}
		} else {
			runner.steerActiveTurn("Update the answer")
		}
		if len(runner.history) != 2 {
			t.Fatalf("history = %s", runner.history)
		}
		var accepted struct {
			Timestamp time.Time
			TurnID    string
			Subtype   string
		}
		if err := json.Unmarshal(runner.history[1], &accepted); err != nil {
			t.Fatal(err)
		}
		if accepted.Subtype != "user_steer" || accepted.TurnID != "turn-1" || !accepted.Timestamp.Before(completedAt) {
			t.Fatalf("typed=%v: acknowledgment moved submitted message after completion: %+v; completed %s", typed, accepted, completedAt)
		}
	}
}

func TestMessageControlSteersWholeTextOnce(t *testing.T) {
	runner := newCodexTestRunner(t)
	runner.active = true
	fake := runner.turnClient.(*fakeCodexTurnClient)
	control := proto.MessageControl{OperationID: "operation-1", Text: "line one\r\nline two\x1b[201~", Mode: "steer"}
	result := runner.submitMessage(control)
	if !result.Accepted || result.Boundary != "provider" || result.OperationID != control.OperationID {
		t.Fatalf("result = %#v", result)
	}
	if len(fake.steered) != 1 || fake.steered[0] != control.Text {
		t.Fatalf("steered = %#v", fake.steered)
	}
}

func TestMessageControlSteerDoesNotStartIdleCodex(t *testing.T) {
	runner := newCodexTestRunner(t)
	result := runner.submitMessage(proto.MessageControl{OperationID: "idle", Text: "hello", Mode: "steer"})
	if result.Accepted || result.Error == "" || runner.active || len(runner.turnClient.(*fakeCodexTurnClient).steered) != 0 {
		t.Fatalf("idle steering started work: %#v", result)
	}
}

func TestMessageControlFailedSteeringIsUnknown(t *testing.T) {
	runner := newCodexTestRunner(t)
	runner.active = true
	runner.turnClient.(*fakeCodexTurnClient).steerErr = errors.New("connection closed")
	result := runner.submitMessage(proto.MessageControl{OperationID: "lost", Text: "hello", Mode: "steer"})
	if result.Accepted || result.Boundary != "unknown" || !strings.Contains(result.Error, "connection closed") {
		t.Fatalf("failed steering = %#v", result)
	}
}

func TestMessageControlClaudeRefusalDoesNotLaunchTurn(t *testing.T) {
	for _, active := range []bool{false, true} {
		runner := &claudeStructuredRunner{active: active}
		result := runner.submitMessage(proto.MessageControl{OperationID: "claude", Text: "hello", Mode: "steer"})
		if result.Accepted || result.Error == "" || runner.active != active {
			t.Fatalf("Claude steering changed turn state: %#v", result)
		}
	}
	runner := &claudeStructuredRunner{active: true}
	result := runner.submitMessage(proto.MessageControl{OperationID: "claude-auto", Text: "hello"})
	if result.Accepted || result.Error == "" {
		t.Fatalf("busy Claude should refuse without hidden queue: %#v", result)
	}
}
