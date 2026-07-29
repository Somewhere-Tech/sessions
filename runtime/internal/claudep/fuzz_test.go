package claudep

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func FuzzParseStreamJSONLine(f *testing.F) {
	f.Add([]byte(`{"type":"assistant","session_id":"session-1","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}`))
	f.Add([]byte(`{"type":"result","subtype":"success","session_id":"session-1","result":"done","usage":{"input_tokens":1,"output_tokens":2}}`))
	f.Add([]byte(`{"type":"future_event","session_id":"session-1","payload":{"kept":true}}`))
	f.Add(largeAssistantEvent(64))

	f.Fuzz(func(t *testing.T, line []byte) {
		event, err := parseStreamJSONLine(line, time.Unix(1, 0), "session-1")
		if err != nil {
			return
		}
		if event.SessionID != "session-1" {
			t.Fatalf("parser accepted an uncorrelated session %q", event.SessionID)
		}
		if event.Type == "" || !json.Valid(event.Raw) {
			t.Fatalf("parser returned malformed normalized event: %#v", event)
		}
		var raw map[string]any
		if err := json.Unmarshal(event.Raw, &raw); err != nil {
			t.Fatal(err)
		}
		if raw["source"] != HistorySource || raw["session_id"] != "session-1" {
			t.Fatalf("normalized history lost source/session correlation: %s", event.Raw)
		}

		again, err := parseStreamJSONLine(event.Raw, time.Unix(2, 0), "session-1")
		if err != nil {
			t.Fatalf("normalized event cannot be parsed again: %v", err)
		}
		if !bytes.Equal(event.Raw, again.Raw) || event.Type != again.Type ||
			event.Subtype != again.Subtype || event.Message != again.Message ||
			!bytes.Equal(event.Usage, again.Usage) {
			t.Fatalf("normalization is not idempotent:\nfirst  %#v\nsecond %#v", event, again)
		}
	})
}

func largeAssistantEvent(blockCount int) []byte {
	content := make([]map[string]any, blockCount)
	for index := range content {
		content[index] = map[string]any{"type": "text", "text": "x"}
	}
	encoded, err := json.Marshal(map[string]any{
		"type":       "assistant",
		"session_id": "session-1",
		"message": map[string]any{
			"role":    "assistant",
			"content": content,
		},
	})
	if err != nil {
		panic(err)
	}
	return encoded
}

func TestParseStreamJSONLineHandlesGiantContentArray(t *testing.T) {
	event, err := parseStreamJSONLine(largeAssistantEvent(8192), time.Unix(1, 0), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(event.Message) != 8192 {
		t.Fatalf("message length = %d, want 8192", len(event.Message))
	}
}

func TestParseStreamJSONLineKeepsExpectedSessionHistoryConsistent(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"assistant","session_id":"session-1","message":{"content":[{"type":"text","text":"kept"}]}}`),
		[]byte(`{"type":"result","session_id":"foreign","result":"poisoned","usage":{}}`),
		[]byte(`{"type":"result","result":"missing session","usage":{}}`),
		[]byte(`{"type":"result","session_id":"session-1","result":"kept","usage":{}}`),
	}
	var accepted []Event
	for _, line := range lines {
		event, err := parseStreamJSONLine(line, time.Unix(1, 0), "session-1")
		if err == nil {
			accepted = append(accepted, event)
		}
	}
	if len(accepted) != 2 || accepted[0].Message != "kept" || accepted[1].Type != "result" || accepted[1].Message != "kept" {
		t.Fatalf("accepted interleaved history = %#v", accepted)
	}
}
