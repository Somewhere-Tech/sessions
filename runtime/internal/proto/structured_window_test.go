package proto

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestStructuredHistoryByteBudgetPreservesNewestAndReleasesOldest(t *testing.T) {
	var history []json.RawMessage
	removed := 0
	for index := 0; index < 10; index++ {
		raw := bytes.Repeat([]byte{byte(index)}, MaxStructuredReplayBytes/4)
		var count int
		history, count = RetainStructuredHistory(history, raw)
		removed += count
	}
	if len(history) != 4 || removed != 6 || history[0][0] != 6 || history[3][0] != 9 {
		t.Fatalf("retained %d records, removed %d", len(history), removed)
	}
	for _, evicted := range history[len(history):cap(history)] {
		if evicted != nil {
			t.Fatal("evicted data remains referenced by backing array")
		}
	}
}

func TestStructuredHistoryOwnsBytesAndKeepsOversizedNewestWhole(t *testing.T) {
	raw := bytes.Repeat([]byte{'x'}, MaxStructuredReplayBytes+1)
	history, removed := RetainStructuredHistory([]json.RawMessage{json.RawMessage(`{"old":true}`)}, raw)
	raw[0] = 'y'
	if removed != 1 || len(history) != 1 || len(history[0]) != len(raw) || history[0][0] != 'x' {
		t.Fatal("newest record was truncated or aliased")
	}
	history, removed = RetainStructuredHistory(history, json.RawMessage(`{"new":true}`))
	if removed != 1 || len(history) != 1 || string(history[0]) != `{"new":true}` {
		t.Fatal("oversized event was not released on the next event")
	}
}

func TestStructuredReplayClientAlsoUsesByteBudget(t *testing.T) {
	var replay replayRequest
	for index := 0; index < 20; index++ {
		replay.appendStructured(bytes.Repeat([]byte{'x'}, MaxStructuredReplayBytes/2))
	}
	if len(replay.cloneStructured()) != 2 {
		t.Fatal("client replay retained an unbounded event payload")
	}
}
