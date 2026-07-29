package watch

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestNormalizeCodexRolloutLine(t *testing.T) {
	context := CodexNormalizeContext{RolloutBasename: "rollout-test.jsonl", LineIndex: 7}
	timestamp := "2026-07-16T12:00:00Z"
	tests := []struct {
		name        string
		raw         string
		wantEvents  []SessionEvent
		wantWorking *bool
	}{
		{
			name: "assistant message",
			raw:  `{"timestamp":"2026-07-16T12:00:00Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"one"},{"type":"input_text","text":"skip"},{"type":"output_text","text":"two"}]}}`,
			wantEvents: []SessionEvent{{
				"type":      "assistant",
				"uuid":      "rollout-test.jsonl:7",
				"timestamp": timestamp,
				"message": map[string]any{
					"role":    "assistant",
					"content": []any{map[string]any{"type": "text", "text": "one\n\ntwo"}},
				},
			}},
		},
		{
			name: "user message",
			raw:  `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}`,
			wantEvents: []SessionEvent{{
				"type": "user",
				"uuid": "rollout-test.jsonl:7",
				"message": map[string]any{
					"role":    "user",
					"content": []any{map[string]any{"type": "text", "text": "hello"}},
				},
			}},
		},
		{
			name:       "user environment preamble filtered",
			raw:        `{"type":"response_item","payload":{"type":"message","role":"user","content":" <environment_context>hidden</environment_context>"}}`,
			wantEvents: nil,
		},
		{
			name:       "developer message filtered",
			raw:        `{"type":"response_item","payload":{"type":"message","role":"developer","content":"hidden"}}`,
			wantEvents: nil,
		},
		{
			name: "function call",
			raw:  `{"timestamp":"2026-07-16T12:00:00Z","type":"response_item","payload":{"type":"function_call","call_id":"call-1","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"}}`,
			wantEvents: []SessionEvent{{
				"type":      "assistant",
				"uuid":      "call-1",
				"timestamp": timestamp,
				"message": map[string]any{
					"role": "assistant",
					"content": []any{map[string]any{
						"type": "tool_use", "id": "call-1", "name": "exec_command", "input": map[string]any{"cmd": "pwd"},
					}},
				},
			}},
		},
		{
			name: "function output",
			raw:  `{"type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":{"ok":true}}}`,
			wantEvents: []SessionEvent{{
				"type": "user",
				"uuid": "rollout-test.jsonl:7:tool_result:call-1",
				"message": map[string]any{
					"role": "user",
					"content": []any{map[string]any{
						"type": "tool_result", "tool_use_id": "call-1", "content": `{"ok":true}`,
					}},
				},
			}},
		},
		{
			name:        "task started",
			raw:         `{"type":"event_msg","payload":{"type":"task_started"}}`,
			wantWorking: boolPointer(true),
		},
		{
			name: "task complete",
			raw:  `{"timestamp":"2026-07-16T12:00:00Z","type":"event_msg","payload":{"type":"task_complete"}}`,
			wantEvents: []SessionEvent{{
				"type":      "assistant",
				"uuid":      "rollout-test.jsonl:7:task_complete",
				"timestamp": timestamp,
				"message": map[string]any{
					"role": "assistant", "content": []any{}, "stop_reason": "end_turn",
				},
			}},
			wantWorking: boolPointer(false),
		},
		{
			name: "nested token count",
			raw:  `{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"inputTokens":10,"output_tokens":4,"cachedInputTokens":3}}}}`,
			wantEvents: []SessionEvent{{
				"type": "assistant",
				"uuid": "rollout-test.jsonl:7:usage",
				"message": map[string]any{
					"role": "assistant", "content": []any{},
					"usage": map[string]any{
						"input_tokens": float64(10), "output_tokens": float64(4), "cache_read_input_tokens": float64(3),
					},
				},
			}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var raw any
			if err := json.Unmarshal([]byte(test.raw), &raw); err != nil {
				t.Fatal(err)
			}
			got := NormalizeCodexRolloutLine(raw, context)
			if !reflect.DeepEqual(got.Events, test.wantEvents) {
				t.Fatalf("events = %#v, want %#v", got.Events, test.wantEvents)
			}
			if !reflect.DeepEqual(got.Working, test.wantWorking) {
				t.Fatalf("working = %v, want %v", pointerValue(got.Working), pointerValue(test.wantWorking))
			}
		})
	}
}

func pointerValue(value *bool) any {
	if value == nil {
		return nil
	}
	return *value
}
