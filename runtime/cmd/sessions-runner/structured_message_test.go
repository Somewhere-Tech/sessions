package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		if accepted.Subtype != "user_steer" || accepted.TurnID != "turn-1" || accepted.Timestamp.After(completedAt) {
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

func TestMessageControlSteerWithoutActiveTurnIsAKnownRefusal(t *testing.T) {
	runner := newCodexTestRunner(t)
	runner.active = true
	runner.turnClient.(*fakeCodexTurnClient).steerErr = fmt.Errorf("%w: conversation \"thread-1\" has no active turn", codexapp.ErrSteerRefused)
	result := runner.submitMessage(proto.MessageControl{OperationID: "refused", Text: "hello", Mode: "steer"})
	if result.Accepted || result.Boundary != "" || !strings.Contains(result.Error, "has no active turn") {
		t.Fatalf("refused steering = %#v", result)
	}
}

// An error answer after the steer was written is not a refusal: Codex may have
// queued the input first.
func TestMessageControlErrorAnswerAfterSteerIsUnknown(t *testing.T) {
	runner := newCodexTestRunner(t)
	runner.active = true
	runner.turnClient.(*fakeCodexTurnClient).steerErr = errors.New("steer Codex turn: JSON-RPC error -32603: internal error")
	result := runner.submitMessage(proto.MessageControl{OperationID: "answered", Text: "hello", Mode: "steer"})
	if result.Accepted || result.Boundary != "unknown" || !strings.Contains(result.Error, "-32603") {
		t.Fatalf("error answer after steer = %#v", result)
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
}

// Steer text with two lines, a tab and multibyte characters, so any
// normalization of the retained text shows up as a mismatch.
const unconfirmedSteerText = "Ändere den Plan:\n\tzweite Zeile — 日本語 ✓"

func historyEventsWithText(t *testing.T, history []json.RawMessage, text string) []map[string]any {
	t.Helper()
	var matches []map[string]any
	for _, raw := range history {
		var event map[string]any
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatal(err)
		}
		message, _ := event["message"].(map[string]any)
		if event["unconfirmedInput"] == text || event["input"] == text || (message != nil && message["content"] == text) {
			matches = append(matches, event)
		}
	}
	return matches
}

// A steer whose outcome Codex never confirmed may still have been applied, so
// shared history must hold its exact text once, marked unconfirmed, on both
// the acknowledged message path and the raw input path. It is never offered
// back as a restorable draft.
func TestUnknownSteerKeepsExactTextInSharedHistoryOnBothPaths(t *testing.T) {
	for _, steerErr := range []error{
		errors.New("steer Codex turn: JSON-RPC error -32603: internal error"),
		context.DeadlineExceeded,
	} {
		for _, typed := range []bool{true, false} {
			runner := newCodexTestRunner(t)
			runner.active = true
			runner.turnClient.(*fakeCodexTurnClient).steerErr = steerErr
			if typed {
				result := runner.submitMessage(proto.MessageControl{OperationID: "op-unknown", Text: unconfirmedSteerText, Mode: "steer"})
				if result.Accepted || result.Boundary != "unknown" {
					t.Fatalf("result = %#v", result)
				}
			} else {
				runner.steerActiveTurn(unconfirmedSteerText)
			}
			events := historyEventsWithText(t, runner.history, unconfirmedSteerText)
			if len(events) != 1 {
				t.Fatalf("typed=%v err=%v: history holds the steer %d times: %s", typed, steerErr, len(events), runner.history)
			}
			event := events[0]
			if event["unconfirmed"] != true || event["unconfirmedInput"] != unconfirmedSteerText {
				t.Fatalf("typed=%v: steer not recorded as unconfirmed exact text: %#v", typed, event)
			}
			if _, restorable := event["input"]; restorable || event["type"] == "user" {
				t.Fatalf("typed=%v: unknown steer recorded as a draft or a delivered message: %#v", typed, event)
			}
			if typed && event["operationId"] != "op-unknown" {
				t.Fatalf("message-path steer lost its operation: %#v", event)
			}
		}
	}
}

// A refusal proved before anything was written is not a message: the
// acknowledged path records no text, so no client can show it as sent.
func TestKnownSteerRefusalAddsNoMessageToSharedHistory(t *testing.T) {
	runner := newCodexTestRunner(t)
	runner.active = true
	runner.turnClient.(*fakeCodexTurnClient).steerErr = fmt.Errorf("%w: no active turn", codexapp.ErrSteerRefused)
	result := runner.submitMessage(proto.MessageControl{OperationID: "op-refused", Text: unconfirmedSteerText, Mode: "steer"})
	if result.Accepted || result.Boundary != "" {
		t.Fatalf("result = %#v", result)
	}
	if events := historyEventsWithText(t, runner.history, unconfirmedSteerText); len(events) != 0 {
		t.Fatalf("known refusal added text to shared history: %#v", events)
	}
}
