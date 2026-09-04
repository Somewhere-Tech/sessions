package claudep

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

const HistorySource = "claude-p-stream-json"

// UserHistoryEvent records accepted composer input in Sessions' existing
// canonical user-message shape.
func UserHistoryEvent(sessionID, text string, at time.Time) (json.RawMessage, error) {
	return marshalHistory(map[string]any{
		"type": "user", "subtype": "user_message", "source": HistorySource,
		"timestamp": historyTimestamp(at), "session_id": sessionID,
		"message": map[string]any{"role": "user", "content": text},
	})
}

// ImportedHistoryEvent makes linked source turns visible in Sessions without
// pretending Claude accepted them as native provider records.
func ImportedHistoryEvent(sessionID, role, text, sourceHistoryID string, at time.Time) (json.RawMessage, error) {
	message := map[string]any{"role": role, "content": text}
	if role == "assistant" {
		message["content"] = []map[string]any{{"type": "text", "text": text}}
	}
	return marshalHistory(map[string]any{
		"type": role, "subtype": "imported_message", "source": "sessions-continuation",
		"timestamp": historyTimestamp(at), "session_id": sessionID,
		"continuedFromHistoryId": sourceHistoryID, "message": message,
	})
}

// ContinuationStartedEvent is the calm, visible boundary before copied
// messages. It lets every client say where this conversation came from and
// which model was chosen before showing the imported history.
func ContinuationStartedEvent(sessionID, detail string, at time.Time) (json.RawMessage, error) {
	return marshalHistory(map[string]any{
		"type": "system", "subtype": "continuation_started", "source": "sessions-continuation",
		"timestamp": historyTimestamp(at), "session_id": sessionID, "detail": detail,
	})
}

// TurnStartedEvent is the authoritative working=true boundary for one
// per-turn Claude process.
func TurnStartedEvent(sessionID string, at time.Time) (json.RawMessage, error) {
	return marshalHistory(map[string]any{
		"type": "claude", "subtype": "turn_started", "source": HistorySource,
		"timestamp": historyTimestamp(at), "session_id": sessionID,
	})
}

// InputRejectedEvent makes steering semantics explicit when a structured
// Claude turn is already active. Sessions never hides a prompt queue behind
// the composer; the user must send again after the current turn finishes.
func InputRejectedEvent(sessionID, message string, at time.Time) (json.RawMessage, error) {
	return marshalHistory(map[string]any{
		"type": "system", "subtype": "input_rejected", "source": HistorySource,
		"timestamp": historyTimestamp(at), "session_id": sessionID,
		"error": message,
	})
}

// FailureHistoryEvent closes the lifecycle when Claude exits without a valid
// result record.
func FailureHistoryEvent(sessionID string, cause error, at time.Time) (json.RawMessage, error) {
	message := "Claude turn failed"
	if cause != nil {
		message = cause.Error()
	}
	return marshalHistory(map[string]any{
		"type": "result", "subtype": "error_during_execution", "source": HistorySource,
		"timestamp": historyTimestamp(at), "session_id": sessionID,
		"is_error": true, "error": message,
	})
}

// NormalizeEvent validates and annotates one native stream-json record. The
// provider payload remains otherwise intact, including assistant content
// blocks, tool_use/tool_result blocks, result text, and token usage.
func NormalizeEvent(raw json.RawMessage, at time.Time) (Event, error) {
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return Event{}, fmt.Errorf("decode Claude stream-json event: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return Event{}, fmt.Errorf("decode Claude stream-json event: %w", err)
	}
	typeName, _ := value["type"].(string)
	if strings.TrimSpace(typeName) == "" {
		return Event{}, fmt.Errorf("Claude stream-json event has no type")
	}
	sessionID, _ := value["session_id"].(string)
	if strings.TrimSpace(sessionID) == "" {
		return Event{}, fmt.Errorf("Claude stream-json event has no session_id")
	}
	if existing, ok := value["source"].(string); ok && existing != "" && existing != HistorySource {
		value["provider_source"] = existing
	}
	value["source"] = HistorySource
	if _, present := value["timestamp"]; !present {
		value["timestamp"] = historyTimestamp(at)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return Event{}, err
	}
	event := Event{Raw: encoded, Type: typeName, SessionID: sessionID}
	event.Subtype, _ = value["subtype"].(string)
	if event.Type == "result" {
		if subtype, present := value["subtype"]; present {
			var ok bool
			event.Subtype, ok = subtype.(string)
			if !ok || strings.TrimSpace(event.Subtype) == "" {
				return Event{}, fmt.Errorf("Claude result event has malformed subtype")
			}
		}
		var ok bool
		event.Message, ok = value["result"].(string)
		if !ok {
			return Event{}, fmt.Errorf("Claude result event has malformed result")
		}
		if isError, present := value["is_error"]; present {
			if _, ok := isError.(bool); !ok {
				return Event{}, fmt.Errorf("Claude result event has malformed is_error")
			}
		}
		if usage, present := value["usage"]; present {
			usageObject, ok := usage.(map[string]any)
			if !ok {
				return Event{}, fmt.Errorf("Claude result event has malformed usage")
			}
			if err := validateUsage(usageObject); err != nil {
				return Event{}, err
			}
			event.Usage, _ = json.Marshal(usage)
		}
	} else if event.Type == "assistant" {
		var err error
		event.Message, err = messageText(value["message"])
		if err != nil {
			return Event{}, err
		}
	}
	return event, nil
}

func parseStreamJSONLine(raw json.RawMessage, at time.Time, expectedSessionID string) (Event, error) {
	if strings.TrimSpace(expectedSessionID) == "" {
		return Event{}, fmt.Errorf("expected Claude session id is empty")
	}
	event, err := NormalizeEvent(raw, at)
	if err != nil {
		return Event{}, err
	}
	if event.SessionID != expectedSessionID {
		return Event{}, fmt.Errorf("Claude session id mismatch: got %s, want %s", event.SessionID, expectedSessionID)
	}
	return event, nil
}

func messageText(raw any) (string, error) {
	message, ok := raw.(map[string]any)
	if !ok {
		return "", fmt.Errorf("Claude assistant event has malformed message")
	}
	content, ok := message["content"].([]any)
	if !ok {
		return "", fmt.Errorf("Claude assistant event has malformed content")
	}
	var result strings.Builder
	for _, item := range content {
		block, ok := item.(map[string]any)
		if !ok {
			return "", fmt.Errorf("Claude assistant event has malformed content block")
		}
		blockType, ok := block["type"].(string)
		if !ok || strings.TrimSpace(blockType) == "" {
			return "", fmt.Errorf("Claude assistant event has content block without a type")
		}
		if blockType != "text" {
			continue
		}
		text, ok := block["text"].(string)
		if !ok {
			return "", fmt.Errorf("Claude assistant text block has malformed text")
		}
		result.WriteString(text)
	}
	return result.String(), nil
}

func validateUsage(usage map[string]any) error {
	for _, field := range []string{
		"input_tokens",
		"output_tokens",
		"cache_creation_input_tokens",
		"cache_read_input_tokens",
	} {
		raw, present := usage[field]
		if !present {
			continue
		}
		number, ok := raw.(json.Number)
		if !ok {
			return fmt.Errorf("Claude result usage field %s is not a number", field)
		}
		count, err := number.Int64()
		if err != nil || count < 0 {
			return fmt.Errorf("Claude result usage field %s is not a non-negative integer", field)
		}
	}
	return nil
}

// HistoryLifecycle decodes the authoritative turn boundaries.
func HistoryLifecycle(raw json.RawMessage) (working bool, authoritative bool) {
	var value struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Source  string `json:"source"`
	}
	if json.Unmarshal(raw, &value) != nil || value.Source != HistorySource {
		return false, false
	}
	if value.Type == "claude" && value.Subtype == "turn_started" {
		return true, true
	}
	if value.Type == "result" {
		return false, true
	}
	// A lane waiting on a permission is not working; the answer restarts it.
	if value.Type == "system" && value.Subtype == "approval_requested" {
		return false, true
	}
	if value.Type == "system" && value.Subtype == "approval_resolved" {
		return true, true
	}
	return false, false
}

// HistoryInitialized reports whether Claude acknowledged the persisted UUID.
func HistoryInitialized(raw json.RawMessage) bool {
	var value struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		Source    string `json:"source"`
		SessionID string `json:"session_id"`
	}
	return json.Unmarshal(raw, &value) == nil && value.Source == HistorySource &&
		value.Type == "system" && value.Subtype == "init" && value.SessionID != ""
}

func marshalHistory(value map[string]any) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	return json.RawMessage(encoded), err
}

func historyTimestamp(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

// ApprovalSummary is the one line a person or a manager sees before deciding
// whether Claude may use a tool: the command for Bash, the file for an edit,
// the tool name otherwise.
func ApprovalSummary(toolName string, input json.RawMessage) string {
	var fields struct {
		Command  string `json:"command"`
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
		URL      string `json:"url"`
	}
	_ = json.Unmarshal(input, &fields)
	switch toolName {
	case "Bash":
		if command := strings.Join(strings.Fields(fields.Command), " "); command != "" {
			if len(command) > 160 {
				command = command[:159] + "…"
			}
			return "Run `" + command + "`"
		}
		return "Run a command"
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		if path := firstNonEmpty(fields.FilePath, fields.Path); path != "" {
			return "Change " + path
		}
		return "Change files"
	case "WebFetch":
		if fields.URL != "" {
			return "Fetch " + fields.URL
		}
		return "Fetch a web page"
	default:
		if toolName == "" {
			return "Use a tool"
		}
		return "Use " + toolName
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// ApprovalRequestedEvent records that a Claude lane is waiting on a permission
// decision; the daemon reads it to mark the session needs-you.
func ApprovalRequestedEvent(sessionID, id, toolName string, input json.RawMessage, at time.Time) (json.RawMessage, error) {
	var command string
	var fields struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(input, &fields) == nil {
		command = fields.Command
	}
	return marshalHistory(map[string]any{
		"type": "system", "subtype": "approval_requested", "source": HistorySource,
		"timestamp": historyTimestamp(at), "session_id": sessionID,
		"approval": map[string]any{
			"id": id, "kind": approvalKind(toolName), "summary": ApprovalSummary(toolName, input),
			"command": command, "tool": toolName, "reason": "",
		},
	})
}

func approvalKind(toolName string) string {
	switch toolName {
	case "Bash":
		return "command"
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return "file-change"
	default:
		return "permissions"
	}
}

// ApprovalResolvedEvent records the answer and who gave it: empty `by` is
// the person, otherwise the session id of the lane that decided.
func ApprovalResolvedEvent(sessionID, id, decision, by string, at time.Time) (json.RawMessage, error) {
	return marshalHistory(map[string]any{
		"type": "system", "subtype": "approval_resolved", "source": HistorySource,
		"timestamp": historyTimestamp(at), "session_id": sessionID,
		"approval": map[string]any{"id": id, "decision": decision, "by": by},
	})
}
