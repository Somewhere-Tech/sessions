package watch

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"unicode"
)

// CodexNormalizeContext makes generated UUIDs stable across a single replay.
type CodexNormalizeContext struct {
	RolloutBasename string
	LineIndex       int
}

// CodexNormalization contains zero or more canonical events plus an optional
// lifecycle state transition.
type CodexNormalization struct {
	Events  []SessionEvent
	Working *bool
}

func stableCodexUUID(context CodexNormalizeContext, suffix string) string {
	base := fmt.Sprintf("%s:%d", context.RolloutBasename, context.LineIndex)
	if suffix != "" {
		return base + ":" + suffix
	}
	return base
}

func recordString(record map[string]any, key string) string {
	value, _ := record[key].(string)
	return value
}

func codexTimestamp(record map[string]any) any {
	if timestamp, ok := record["timestamp"].(string); ok {
		return timestamp
	}
	return nil
}

func addOptionalTimestamp(event SessionEvent, line map[string]any) {
	if timestamp := codexTimestamp(line); timestamp != nil {
		event["timestamp"] = timestamp
	}
}

func codexContentText(content any, wantedType string) string {
	if text, ok := content.(string); ok {
		return strings.TrimSpace(text)
	}
	blocks, ok := content.([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok || block["type"] != wantedType {
			continue
		}
		if text, ok := block["text"].(string); ok && text != "" {
			parts = append(parts, text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func isCodexUserPreamble(text string) bool {
	trimmed := strings.TrimLeftFunc(text, unicode.IsSpace)
	return strings.HasPrefix(trimmed, "<environment_context>") ||
		(strings.Contains(trimmed, "<approval_policy>") && strings.Contains(trimmed, "<sandbox")) ||
		(strings.Contains(trimmed, "<sandbox_mode>") && strings.Contains(trimmed, "<cwd>"))
}

func parseCodexToolInput(raw any) map[string]any {
	text, ok := raw.(string)
	if !ok {
		return map[string]any{}
	}
	var parsed map[string]any
	if json.Unmarshal([]byte(text), &parsed) != nil || parsed == nil {
		return map[string]any{}
	}
	return parsed
}

func codexOutputString(output any) string {
	if text, ok := output.(string); ok {
		return text
	}
	if output == nil {
		return ""
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return fmt.Sprint(output)
	}
	return string(encoded)
}

func finiteNumber(record map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		value, ok := numericValue(record[key])
		if ok && !math.IsNaN(value) && !math.IsInf(value, 0) {
			return value, true
		}
	}
	return 0, false
}

func numericValue(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case float32:
		return float64(number), true
	case int:
		return float64(number), true
	case int8:
		return float64(number), true
	case int16:
		return float64(number), true
	case int32:
		return float64(number), true
	case int64:
		return float64(number), true
	case uint:
		return float64(number), true
	case uint8:
		return float64(number), true
	case uint16:
		return float64(number), true
	case uint32:
		return float64(number), true
	case uint64:
		return float64(number), true
	case json.Number:
		parsed, err := number.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func usageFromRecord(record map[string]any) map[string]any {
	usage := make(map[string]any)
	if value, ok := finiteNumber(record, "input_tokens", "inputTokens", "input"); ok {
		usage["input_tokens"] = value
	}
	if value, ok := finiteNumber(record, "output_tokens", "outputTokens", "output"); ok {
		usage["output_tokens"] = value
	}
	if value, ok := finiteNumber(record, "cache_creation_input_tokens", "cacheCreationInputTokens"); ok {
		usage["cache_creation_input_tokens"] = value
	}
	if value, ok := finiteNumber(record,
		"cache_read_input_tokens", "cacheReadInputTokens", "cached_input_tokens", "cachedInputTokens"); ok {
		usage["cache_read_input_tokens"] = value
	}
	if len(usage) == 0 {
		return nil
	}
	return usage
}

func usageFromUnknown(value any, depth int) map[string]any {
	record, ok := value.(map[string]any)
	if !ok || depth > 3 {
		return nil
	}
	if usage := usageFromRecord(record); usage != nil {
		return usage
	}
	for _, key := range []string{
		"usage", "tokens", "token_count", "tokenCount", "token_usage", "tokenUsage",
		"total_token_usage", "totalTokenUsage", "last_token_usage", "lastTokenUsage", "info",
	} {
		if usage := usageFromUnknown(record[key], depth+1); usage != nil {
			return usage
		}
	}
	return nil
}

func normalizeCodexMessage(payload, line map[string]any, context CodexNormalizeContext) []SessionEvent {
	switch recordString(payload, "role") {
	case "developer":
		return nil
	case "assistant":
		text := codexContentText(payload["content"], "output_text")
		if text == "" {
			return nil
		}
		event := SessionEvent{
			"type": "assistant",
			"uuid": stableCodexUUID(context, ""),
			"message": map[string]any{
				"role":    "assistant",
				"content": []any{map[string]any{"type": "text", "text": text}},
			},
		}
		addOptionalTimestamp(event, line)
		return []SessionEvent{event}
	case "user":
		text := codexContentText(payload["content"], "input_text")
		if text == "" || isCodexUserPreamble(text) {
			return nil
		}
		event := SessionEvent{
			"type": "user",
			"uuid": stableCodexUUID(context, ""),
			"message": map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "text", "text": text}},
			},
		}
		addOptionalTimestamp(event, line)
		return []SessionEvent{event}
	default:
		return nil
	}
}

func normalizeCodexFunctionCall(payload, line map[string]any) []SessionEvent {
	callID := recordString(payload, "call_id")
	name := recordString(payload, "name")
	if callID == "" || name == "" {
		return nil
	}
	event := SessionEvent{
		"type": "assistant",
		"uuid": callID,
		"message": map[string]any{
			"role": "assistant",
			"content": []any{map[string]any{
				"type":  "tool_use",
				"id":    callID,
				"name":  name,
				"input": parseCodexToolInput(payload["arguments"]),
			}},
		},
	}
	addOptionalTimestamp(event, line)
	return []SessionEvent{event}
}

func normalizeCodexFunctionOutput(payload, line map[string]any, context CodexNormalizeContext) []SessionEvent {
	callID := recordString(payload, "call_id")
	if callID == "" {
		return nil
	}
	event := SessionEvent{
		"type": "user",
		"uuid": stableCodexUUID(context, "tool_result:"+callID),
		"message": map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type":        "tool_result",
				"tool_use_id": callID,
				"content":     codexOutputString(payload["output"]),
			}},
		},
	}
	addOptionalTimestamp(event, line)
	return []SessionEvent{event}
}

func normalizeCodexTokenCount(payload, line map[string]any, context CodexNormalizeContext) []SessionEvent {
	usage := usageFromUnknown(payload, 0)
	if usage == nil {
		return nil
	}
	event := SessionEvent{
		"type": "assistant",
		"uuid": stableCodexUUID(context, "usage"),
		"message": map[string]any{
			"role":    "assistant",
			"content": []any{},
			"usage":   usage,
		},
	}
	addOptionalTimestamp(event, line)
	return []SessionEvent{event}
}

func normalizeCodexTaskComplete(line map[string]any, context CodexNormalizeContext) []SessionEvent {
	event := SessionEvent{
		"type": "assistant",
		"uuid": stableCodexUUID(context, "task_complete"),
		"message": map[string]any{
			"role":        "assistant",
			"content":     []any{},
			"stop_reason": "end_turn",
		},
	}
	addOptionalTimestamp(event, line)
	return []SessionEvent{event}
}

func boolPointer(value bool) *bool {
	return &value
}

// NormalizeCodexRolloutLine maps a decoded rollout line to canonical
// Claude-shaped API events.
func NormalizeCodexRolloutLine(raw any, context CodexNormalizeContext) CodexNormalization {
	line, ok := raw.(map[string]any)
	if !ok {
		return CodexNormalization{}
	}
	lineType := recordString(line, "type")
	payload, ok := line["payload"].(map[string]any)
	if lineType == "" || !ok {
		return CodexNormalization{}
	}

	if lineType == "response_item" {
		switch recordString(payload, "type") {
		case "message":
			return CodexNormalization{Events: normalizeCodexMessage(payload, line, context)}
		case "function_call":
			return CodexNormalization{Events: normalizeCodexFunctionCall(payload, line)}
		case "function_call_output":
			return CodexNormalization{Events: normalizeCodexFunctionOutput(payload, line, context)}
		}
		return CodexNormalization{}
	}

	if lineType == "event_msg" {
		switch recordString(payload, "type") {
		case "task_started":
			return CodexNormalization{Working: boolPointer(true)}
		case "task_complete":
			return CodexNormalization{
				Events:  normalizeCodexTaskComplete(line, context),
				Working: boolPointer(false),
			}
		case "token_count":
			return CodexNormalization{Events: normalizeCodexTokenCount(payload, line, context)}
		}
	}
	return CodexNormalization{}
}
