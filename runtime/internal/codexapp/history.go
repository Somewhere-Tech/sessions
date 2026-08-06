package codexapp

import (
	"encoding/json"
	"time"
)

const HistorySource = "codex-app-server"

// UserHistoryEvent maps an accepted SendUserTurn input into the normalized
// event shape already consumed by Sessions' last/transcript API and CLI.
func UserHistoryEvent(conversationID, text string, at time.Time) (json.RawMessage, error) {
	return marshalHistory(map[string]any{
		"type":           "user",
		"subtype":        "user_message",
		"source":         HistorySource,
		"timestamp":      historyTimestamp(at),
		"conversationId": conversationID,
		"message": map[string]any{
			"role": "user", "content": text,
		},
	})
}

// ImportedHistoryEvent mirrors one authored source message into Sessions'
// normalized destination history after app-server accepts the same message.
// The source marker lets the UI explain that these turns predate this runtime.
func ImportedHistoryEvent(conversationID, role, text, sourceHistoryID string, at time.Time) (json.RawMessage, error) {
	message := map[string]any{"role": role, "content": text}
	if role == "assistant" {
		message["content"] = []map[string]any{{"type": "text", "text": text}}
	}
	return marshalHistory(map[string]any{
		"type": role, "subtype": "imported_message", "source": "sessions-continuation",
		"timestamp": historyTimestamp(at), "conversationId": conversationID,
		"continuedFromHistoryId": sourceHistoryID, "message": message,
	})
}

// InputRejectedEvent makes steering semantics explicit when a structured
// Codex turn is already active. Sessions never hides a prompt queue behind
// the composer, and it never drops composed text without saying so; the user
// must send again after the current turn finishes. The shape mirrors
// claudep.InputRejectedEvent so both structured providers report a refused
// message the same way.
func InputRejectedEvent(conversationID, message string, at time.Time) (json.RawMessage, error) {
	return marshalHistory(map[string]any{
		"type": "system", "subtype": "input_rejected", "source": HistorySource,
		"timestamp": historyTimestamp(at), "conversationId": conversationID,
		"error": message,
	})
}

// HistoryEvent preserves every structured app-server notification while
// projecting completed assistant items into the existing message shape.
func HistoryEvent(event Event, at time.Time) (json.RawMessage, error) {
	value := map[string]any{
		"type":      "codex",
		"source":    HistorySource,
		"timestamp": historyTimestamp(at),
	}
	switch event := event.(type) {
	case TurnStarted:
		value["subtype"] = "turn_started"
		value["conversationId"] = event.ConversationID
		value["turnId"] = event.TurnID
	case AgentMessageDelta:
		value["subtype"] = "agent_message_delta"
		value["conversationId"] = event.ConversationID
		value["turnId"] = event.TurnID
		value["itemId"] = event.ItemID
		value["delta"] = event.Delta
	case ItemStarted:
		value["subtype"] = "item_started"
		value["conversationId"] = event.ConversationID
		value["turnId"] = event.TurnID
		value["item"] = historyItem(event.Item)
		if event.StartedAtMS > 0 {
			value["timestamp"] = historyTimestamp(time.UnixMilli(event.StartedAtMS))
		}
	case ItemCompleted:
		value["subtype"] = "item_completed"
		value["conversationId"] = event.ConversationID
		value["turnId"] = event.TurnID
		value["item"] = historyItem(event.Item)
		if event.CompletedAtMS > 0 {
			value["timestamp"] = historyTimestamp(time.UnixMilli(event.CompletedAtMS))
		}
		commentary := event.Item.Phase != nil && *event.Item.Phase == "commentary"
		if event.Item.Type == "agentMessage" && event.Item.Text != "" && !commentary {
			value["type"] = "assistant"
			value["message"] = map[string]any{
				"role":    "assistant",
				"content": []map[string]any{{"type": "text", "text": event.Item.Text}},
			}
		}
	case PlanUpdated:
		value["subtype"] = "plan_updated"
		value["conversationId"] = event.ConversationID
		value["turnId"] = event.TurnID
		value["explanation"] = event.Explanation
		value["plan"] = event.Plan
	case TokenCount:
		value["subtype"] = "token_count"
		value["conversationId"] = event.ConversationID
		value["turnId"] = event.TurnID
		value["usage"] = event.Usage
	case TurnComplete:
		value["subtype"] = "turn_completed"
		value["conversationId"] = event.ConversationID
		value["turnId"] = event.TurnID
		value["status"] = event.Status
		if event.Error != nil {
			value["error"] = event.Error
		}
	default:
		value["subtype"] = "unknown"
	}
	return marshalHistory(value)
}

// historyItem preserves the complete provider item payload. ThreadItem keeps
// its item-specific fields in Raw so the protocol parser can stay generated
// from a small common subset; serializing the struct directly would discard
// command output, file diffs, MCP results, reasoning summaries, and other GUI
// material before it reached the session history stream.
func historyItem(item ThreadItem) any {
	if len(item.Raw) > 0 {
		var value map[string]any
		if json.Unmarshal(item.Raw, &value) == nil && value != nil {
			return value
		}
	}
	return item
}

// HistoryLifecycle returns the authoritative working state carried by a
// normalized structured event.
func HistoryLifecycle(raw json.RawMessage) (working bool, authoritative bool) {
	var value struct {
		Source  string `json:"source"`
		Subtype string `json:"subtype"`
	}
	if json.Unmarshal(raw, &value) != nil || value.Source != HistorySource {
		return false, false
	}
	switch value.Subtype {
	case "turn_started":
		return true, true
	case "turn_completed":
		return false, true
	default:
		return false, false
	}
}

func marshalHistory(value map[string]any) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	return json.RawMessage(encoded), err
}

func historyTimestamp(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}
