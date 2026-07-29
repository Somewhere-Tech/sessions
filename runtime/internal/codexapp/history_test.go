package codexapp

import (
	"encoding/json"
	"testing"
	"time"
)

func TestHistoryEventsPreserveSourceMessagesAndLifecycle(t *testing.T) {
	user, err := UserHistoryEvent("thread-1", "hello", time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	var userValue map[string]any
	if err := json.Unmarshal(user, &userValue); err != nil {
		t.Fatal(err)
	}
	if userValue["type"] != "user" || userValue["source"] != HistorySource {
		t.Fatalf("user history = %#v", userValue)
	}

	started, err := HistoryEvent(TurnStarted{ConversationID: "thread-1", TurnID: "turn-1"}, time.Unix(2, 0))
	if err != nil {
		t.Fatal(err)
	}
	if working, authoritative := HistoryLifecycle(started); !authoritative || !working {
		t.Fatalf("started lifecycle = working:%v authoritative:%v", working, authoritative)
	}

	phase := "final_answer"
	assistant, err := HistoryEvent(ItemCompleted{
		ConversationID: "thread-1", TurnID: "turn-1",
		Item: ThreadItem{ID: "message-1", Type: "agentMessage", Text: "OK", Phase: &phase},
	}, time.Unix(3, 0))
	if err != nil {
		t.Fatal(err)
	}
	var assistantValue map[string]any
	if err := json.Unmarshal(assistant, &assistantValue); err != nil {
		t.Fatal(err)
	}
	message, _ := assistantValue["message"].(map[string]any)
	if assistantValue["type"] != "assistant" || message["role"] != "assistant" {
		t.Fatalf("assistant history = %#v", assistantValue)
	}

	completed, err := HistoryEvent(TurnComplete{ConversationID: "thread-1", TurnID: "turn-1", Status: "completed"}, time.Unix(4, 0))
	if err != nil {
		t.Fatal(err)
	}
	if working, authoritative := HistoryLifecycle(completed); !authoritative || working {
		t.Fatalf("completed lifecycle = working:%v authoritative:%v", working, authoritative)
	}
}

func TestHistoryEventPreservesCompleteThreadItemForGUI(t *testing.T) {
	raw := json.RawMessage(`{
		"id":"command-1",
		"type":"commandExecution",
		"command":"go test ./...",
		"cwd":"/tmp/project",
		"status":"completed",
		"aggregatedOutput":"ok\n",
		"exitCode":0,
		"durationMs":125
	}`)
	var item ThreadItem
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatal(err)
	}
	event, err := HistoryEvent(ItemCompleted{
		ConversationID: "thread-1",
		TurnID:         "turn-1",
		Item:           item,
	}, time.Unix(3, 0))
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		Item map[string]any `json:"item"`
	}
	if err := json.Unmarshal(event, &value); err != nil {
		t.Fatal(err)
	}
	if value.Item["command"] != "go test ./..." || value.Item["aggregatedOutput"] != "ok\n" || value.Item["exitCode"] != float64(0) {
		t.Fatalf("GUI item fields were not preserved: %#v", value.Item)
	}
}
