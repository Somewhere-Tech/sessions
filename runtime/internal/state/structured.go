package state

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
)

func (s *Session) recordCodexLocked(event *proto.Event) int64 {
	event.ClaudeEvent = append(json.RawMessage(nil), event.CodexEvent...)
	return s.recordClaudeLocked(event)
}

func (s *Session) recordClaudeLocked(event *proto.Event) int64 {
	event.ClaudeIndex = s.claudeBase + int64(len(s.claude))
	raw := append(json.RawMessage(nil), event.ClaudeEvent...)
	providerActivityAt := time.Now().UnixMilli()
	s.claude = append(s.claude, raw)
	if len(s.claude) > maxClaudeEvents {
		removed := len(s.claude) - maxClaudeEvents
		s.claude = append([]json.RawMessage(nil), s.claude[removed:]...)
		s.claudeBase += int64(removed)
	}

	var value map[string]any
	if json.Unmarshal(raw, &value) != nil {
		return providerActivityAt
	}
	switch timestamp := value["timestamp"].(type) {
	case float64:
		if timestamp > 0 {
			providerActivityAt = int64(timestamp)
		}
	case string:
		if parsed, err := time.Parse(time.RFC3339Nano, timestamp); err == nil {
			providerActivityAt = parsed.UnixMilli()
		}
	}
	switch value["type"] {
	case "custom-title":
		if title, ok := value["customTitle"].(string); ok && title != "" {
			s.info.ClaudeCustomTitle = title
		}
	case "ai-title":
		if title, ok := value["aiTitle"].(string); ok && title != "" {
			s.info.ClaudeAITitle = title
		}
	}
	if !realUserMessage(value) {
		return providerActivityAt
	}
	timestamp, ok := value["timestamp"].(string)
	if !ok {
		return providerActivityAt
	}
	parsed, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return providerActivityAt
	}
	millis := parsed.UnixMilli()
	if s.info.LastUserMessageAt == nil || millis > *s.info.LastUserMessageAt {
		s.info.LastUserMessageAt = &millis
	}
	return providerActivityAt
}

func realUserMessage(event map[string]any) bool {
	if event["type"] != "user" {
		return false
	}
	message, ok := event["message"].(map[string]any)
	if !ok || message["role"] != "user" {
		return false
	}
	var text string
	switch content := message["content"].(type) {
	case string:
		text = content
	case []any:
		if len(content) == 0 {
			return false
		}
		allToolResults := true
		for _, item := range content {
			block, ok := item.(map[string]any)
			if !ok || block["type"] != "tool_result" {
				allToolResults = false
			}
			if block["type"] == "text" && text == "" {
				text, _ = block["text"].(string)
			}
		}
		if allToolResults {
			return false
		}
	default:
		return false
	}
	trimmed := strings.TrimLeft(text, " \t\r\n")
	for _, prefix := range []string{"<", "Caveat:", "This session is being continued", "[Request interrupted"} {
		if strings.HasPrefix(trimmed, prefix) {
			return false
		}
	}
	return true
}

func structuredSnapshot(events []json.RawMessage) string {
	var output strings.Builder
	for _, raw := range events {
		var event map[string]any
		if json.Unmarshal(raw, &event) != nil {
			continue
		}
		message, ok := event["message"].(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		if role != "user" && role != "assistant" {
			continue
		}
		text := structuredContentText(message["content"])
		if strings.TrimSpace(text) == "" {
			continue
		}
		if output.Len() > 0 {
			output.WriteString("\n\n")
		}
		fmt.Fprintf(&output, "[%s]\n%s", role, text)
	}
	return output.String()
}

func structuredContentText(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	blocks, ok := content.([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, value := range blocks {
		block, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := block["text"].(string); ok && text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "")
}
