package codexapp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func FuzzDecodeJSONRPC(f *testing.F) {
	f.Add([]byte(`{"id":1,"result":{"ok":true}}`))
	f.Add([]byte(`{"method":"future/event","params":{"nested":[1,2,3]}}`))
	f.Add([]byte(`{"id":"request-1","method":"future/request","params":null}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		message, err := decodeJSONRPC(data)
		if err != nil {
			return
		}
		if !json.Valid(data) {
			t.Fatal("decoder accepted invalid JSON")
		}
		if message.Method == "" && len(message.ID) == 0 {
			t.Fatal("decoder accepted an envelope without a method or id")
		}
		if len(message.ID) > 0 && !validJSONRPCID(message.ID) {
			t.Fatalf("decoder returned an invalid id: %s", message.ID)
		}
	})
}

func FuzzParseServerEvent(f *testing.F) {
	f.Add("item/agentMessage/delta", []byte(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"hello"}`))
	f.Add("item/completed", []byte(`{"threadId":"thread-1","turnId":"turn-1","completedAtMs":1,"item":{"id":"item-1","type":"agentMessage","text":"done"}}`))
	f.Add("turn/completed", []byte(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed","items":[]}}`))

	f.Fuzz(func(t *testing.T, method string, params []byte) {
		parsed, err := parseServerEvent(method, params)
		if err != nil {
			return
		}
		if parsed.event == nil {
			t.Fatal("server-event parser succeeded without an event")
		}
		encoded, err := json.Marshal(parsed.event)
		if err != nil || !json.Valid(encoded) {
			t.Fatalf("parsed event is not JSON-marshalable: %v", err)
		}
		history, err := HistoryEvent(parsed.event, time.Unix(1, 0))
		if err != nil || !json.Valid(history) {
			t.Fatalf("parsed event cannot be recorded safely: %v", err)
		}
		assertCorrelatedServerEvent(t, parsed.event)
		for _, item := range parsed.items {
			if err := validateThreadItem(item); err != nil {
				t.Fatalf("parser returned malformed completion item: %v", err)
			}
		}
	})
}

func assertCorrelatedServerEvent(t *testing.T, event Event) {
	t.Helper()
	var conversationID, turnID string
	switch event := event.(type) {
	case AgentMessageDelta:
		conversationID, turnID = event.ConversationID, event.TurnID
		if event.ItemID == "" {
			t.Fatal("parsed agent delta has no item id")
		}
	case ItemStarted:
		conversationID, turnID = event.ConversationID, event.TurnID
	case ItemCompleted:
		conversationID, turnID = event.ConversationID, event.TurnID
	case TokenCount:
		conversationID, turnID = event.ConversationID, event.TurnID
	case TurnComplete:
		conversationID, turnID = event.ConversationID, event.TurnID
	default:
		t.Fatalf("unexpected parsed event type %T", event)
	}
	if conversationID == "" || turnID == "" {
		t.Fatalf("parsed event is not correlated: %#v", event)
	}
}

func TestEventQueueConcurrentProducerConsumer(t *testing.T) {
	const eventCount = 5000
	state := newTurnState("thread-1")
	if !state.acceptTurnID("turn-1") {
		t.Fatal("failed to initialize turn id")
	}
	stream := state.stream()
	consumed := make(chan int, 1)
	go func() {
		count := 0
		for range stream.Events {
			count++
		}
		consumed <- count
	}()
	go func() {
		for index := 0; index < eventCount; index++ {
			state.emit(AgentMessageDelta{
				ConversationID: "thread-1",
				TurnID:         "turn-1",
				ItemID:         fmt.Sprintf("item-%d", index),
				Delta:          "x",
			})
		}
		state.complete(TurnComplete{
			ConversationID: "thread-1",
			TurnID:         "turn-1",
			Status:         "completed",
		}, nil)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := stream.Result(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" {
		t.Fatalf("result status = %q", result.Status)
	}
	select {
	case count := <-consumed:
		if count != eventCount+1 {
			t.Fatalf("consumed %d events, want %d", count, eventCount+1)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
