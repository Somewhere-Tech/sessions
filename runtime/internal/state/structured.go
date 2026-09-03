package state

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/providerfault"
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
	s.trackProviderFaultLocked(value, providerActivityAt)
	switch value["type"] {
	case "system":
		s.trackApprovalLocked(value, providerActivityAt)
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

func (s *Session) trackProviderFaultLocked(value map[string]any, at int64) {
	if value["type"] == "system" && value["subtype"] == "provider_fault" {
		fault := providerfault.Fault{}
		fault.Kind, _ = value["kind"].(string)
		fault.Detail, _ = value["detail"].(string)
		provider, _ := value["provider"].(string)
		s.setProviderFaultLocked(provider, fault, at)
		return
	}
	if fault, ok := nativeClaudeProviderFault(value); ok {
		s.setProviderFaultLocked("claude", fault, at)
		return
	}
	if successfulProviderTurn(value) {
		s.clearProviderFaultLocked()
	}
}

func nativeClaudeProviderFault(value map[string]any) (providerfault.Fault, bool) {
	isError, _ := value["isApiErrorMessage"].(bool)
	if !isError {
		isError, _ = value["is_api_error_message"].(bool)
	}
	if !isError || value["type"] != "assistant" {
		return providerfault.Fault{}, false
	}
	message, _ := value["message"].(map[string]any)
	text := structuredContentText(message["content"])
	status := numericStatus(value["apiErrorStatus"])
	if status == 0 {
		status = numericStatus(value["api_error_status"])
	}
	return providerfault.Classify("claude", text, status), true
}

func numericStatus(value any) int {
	switch number := value.(type) {
	case float64:
		return int(number)
	case json.Number:
		status, _ := number.Int64()
		return int(status)
	default:
		return 0
	}
}

func (s *Session) setProviderFaultLocked(provider string, fault providerfault.Fault, at int64) {
	s.info.FailureKind = fault.Kind
	s.info.FailureDetail = fault.Detail
	s.info.FailureProvider = provider
	s.info.FailureAt = at
	if fault.Detail != "" {
		s.info.LastSummary = fault.Detail
	}
}

func successfulProviderTurn(value map[string]any) bool {
	source, _ := value["source"].(string)
	if source == "codex-app-server" && value["subtype"] == "turn_completed" {
		status, _ := value["status"].(string)
		return strings.EqualFold(status, "completed")
	}
	if source != "claude-p-stream-json" || value["type"] != "result" {
		return false
	}
	isError, _ := value["is_error"].(bool)
	return !isError
}

func (s *Session) clearProviderFaultLocked() {
	s.info.FailureKind = ""
	s.info.FailureDetail = ""
	s.info.FailureProvider = ""
	s.info.FailureAt = 0
}

// ProviderFault is the policy seam for a future retry controller. It returns
// only classified provider state and does not decide whether or when to retry.
func (s *Session) ProviderFault() (providerfault.Fault, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.info.FailureKind == "" || s.info.FailureDetail == "" {
		return providerfault.Fault{}, false
	}
	fault := providerfault.Classify(s.info.FailureProvider, s.info.FailureDetail, 0)
	fault.Kind = s.info.FailureKind
	fault.Detail = s.info.FailureDetail
	return fault, true
}

func (s *Session) SetProviderFault(provider string, fault providerfault.Fault, at int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setProviderFaultLocked(provider, fault, at)
}

func (s *Session) ClearProviderFault() {
	s.mu.Lock()
	s.clearProviderFaultLocked()
	s.mu.Unlock()
}

func realUserMessage(event map[string]any) bool {
	if event["type"] != "user" {
		return false
	}
	// A provider writes its own scheduled injections into the transcript as
	// user records and marks them isMeta. Verified on the owner's orchestrator
	// transcript: its 30-minute "INTEGRATOR TICK" carries isMeta true, against
	// 206 ordinary user records in the same window that do not. Counting one as
	// a person is what let a tick land three seconds after he spoke and become
	// the only record of "last user contact" the product had.
	//
	// LastHumanMessageAt is the authoritative answer for a live session because
	// it is stamped where Sessions itself sees the input. This check is what
	// makes the transcript-derived field honest too, which is the only signal
	// available for a conversation with no live session -- an ended one, or one
	// read on another machine.
	if meta, ok := event["isMeta"].(bool); ok && meta {
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

// trackApprovalLocked keeps PendingApproval in step with the structured
// stream: a requested approval is pending until the runner records its
// resolution. Replayed history goes through the same path, so a daemon that
// reconnects to a runner still holding a request shows it.
func (s *Session) trackApprovalLocked(value map[string]any, at int64) {
	approval, _ := value["approval"].(map[string]any)
	text := func(key string) string {
		if v, ok := approval[key].(string); ok {
			return v
		}
		return ""
	}
	switch value["subtype"] {
	case "approval_requested":
		if text("id") == "" {
			return
		}
		s.info.PendingApproval = &ApprovalPrompt{
			ID: text("id"), Kind: text("kind"), Summary: text("summary"),
			Command: text("command"), Cwd: text("cwd"), Reason: text("reason"), At: at,
		}
	case "approval_resolved":
		if s.info.PendingApproval != nil && (text("id") == "" || s.info.PendingApproval.ID == text("id")) {
			s.info.PendingApproval = nil
		}
	}
}
