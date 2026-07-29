package codexapp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

var errUnsupportedServerEvent = errors.New("unsupported app-server event")

type parsedServerEvent struct {
	event Event
	items []ThreadItem
}

// decodeJSONRPC decodes one complete app-server frame and rejects envelopes
// that cannot be safely correlated as a request, response, or notification.
func decodeJSONRPC(data []byte) (wireMessage, error) {
	var message wireMessage
	if err := json.Unmarshal(data, &message); err != nil {
		return wireMessage{}, fmt.Errorf("decode JSON-RPC message: %w", err)
	}
	if len(message.ID) > 0 {
		id := bytes.TrimSpace(message.ID)
		if bytes.Equal(id, []byte("null")) {
			message.ID = nil
		} else if !validJSONRPCID(id) {
			return wireMessage{}, errors.New("decode JSON-RPC message: id must be a string or number")
		}
	}
	if message.Method == "" && len(message.ID) == 0 {
		return wireMessage{}, errors.New("decode JSON-RPC message: missing method and id")
	}
	return message, nil
}

func validJSONRPCID(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var id any
	if err := decoder.Decode(&id); err != nil {
		return false
	}
	switch id.(type) {
	case string, json.Number:
		return true
	default:
		return false
	}
}

// parseServerEvent converts a recognized notification payload into the public
// event shape only after all identifiers and nested values needed to mutate a
// turn have been validated.
func parseServerEvent(method string, params json.RawMessage) (parsedServerEvent, error) {
	switch method {
	case "item/agentMessage/delta":
		var notification AgentMessageDeltaNotification
		if err := json.Unmarshal(params, &notification); err != nil {
			return parsedServerEvent{}, err
		}
		if err := validateEventIDs(notification.ThreadID, notification.TurnID); err != nil {
			return parsedServerEvent{}, err
		}
		if notification.ItemID == "" {
			return parsedServerEvent{}, errors.New("app-server agent-message delta has no item id")
		}
		return parsedServerEvent{event: AgentMessageDelta{
			ConversationID: notification.ThreadID,
			TurnID:         notification.TurnID,
			ItemID:         notification.ItemID,
			Delta:          notification.Delta,
		}}, nil

	case "item/started":
		var notification ItemStartedNotification
		if err := json.Unmarshal(params, &notification); err != nil {
			return parsedServerEvent{}, err
		}
		if err := validateEventIDs(notification.ThreadID, notification.TurnID); err != nil {
			return parsedServerEvent{}, err
		}
		if err := validateThreadItem(notification.Item); err != nil {
			return parsedServerEvent{}, err
		}
		if notification.StartedAtMS < 0 {
			return parsedServerEvent{}, errors.New("app-server item has a negative start timestamp")
		}
		return parsedServerEvent{event: ItemStarted{
			ConversationID: notification.ThreadID,
			TurnID:         notification.TurnID,
			StartedAtMS:    notification.StartedAtMS,
			Item:           notification.Item,
		}}, nil

	case "item/completed":
		var notification ItemCompletedNotification
		if err := json.Unmarshal(params, &notification); err != nil {
			return parsedServerEvent{}, err
		}
		if err := validateEventIDs(notification.ThreadID, notification.TurnID); err != nil {
			return parsedServerEvent{}, err
		}
		if err := validateThreadItem(notification.Item); err != nil {
			return parsedServerEvent{}, err
		}
		if notification.CompletedAtMS < 0 {
			return parsedServerEvent{}, errors.New("app-server item has a negative completion timestamp")
		}
		return parsedServerEvent{event: ItemCompleted{
			ConversationID: notification.ThreadID,
			TurnID:         notification.TurnID,
			CompletedAtMS:  notification.CompletedAtMS,
			Item:           notification.Item,
		}}, nil

	case "turn/plan/updated":
		var notification struct {
			ThreadID    string         `json:"threadId"`
			TurnID      string         `json:"turnId"`
			Explanation *string        `json:"explanation"`
			Plan        []TurnPlanStep `json:"plan"`
		}
		if err := json.Unmarshal(params, &notification); err != nil {
			return parsedServerEvent{}, err
		}
		if err := validateEventIDs(notification.ThreadID, notification.TurnID); err != nil {
			return parsedServerEvent{}, err
		}
		for _, step := range notification.Plan {
			if step.Step == "" || step.Status == "" {
				return parsedServerEvent{}, errors.New("app-server plan step is incomplete")
			}
		}
		return parsedServerEvent{event: PlanUpdated{
			ConversationID: notification.ThreadID,
			TurnID:         notification.TurnID,
			Explanation:    notification.Explanation,
			Plan:           notification.Plan,
		}}, nil

	case "thread/tokenUsage/updated":
		var notification ThreadTokenUsageUpdatedNotification
		if err := json.Unmarshal(params, &notification); err != nil {
			return parsedServerEvent{}, err
		}
		if err := validateEventIDs(notification.ThreadID, notification.TurnID); err != nil {
			return parsedServerEvent{}, err
		}
		if err := validateTokenUsage(notification.TokenUsage); err != nil {
			return parsedServerEvent{}, err
		}
		return parsedServerEvent{event: TokenCount{
			ConversationID: notification.ThreadID,
			TurnID:         notification.TurnID,
			Usage:          notification.TokenUsage,
		}}, nil

	case "turn/completed":
		var notification TurnCompletedNotification
		if err := json.Unmarshal(params, &notification); err != nil {
			return parsedServerEvent{}, err
		}
		if err := validateEventIDs(notification.ThreadID, notification.Turn.ID); err != nil {
			return parsedServerEvent{}, err
		}
		if notification.Turn.Status == "" {
			return parsedServerEvent{}, errors.New("app-server completed turn has no status")
		}
		for _, item := range notification.Turn.Items {
			if err := validateThreadItem(item); err != nil {
				return parsedServerEvent{}, err
			}
		}
		return parsedServerEvent{
			event: TurnComplete{
				ConversationID: notification.ThreadID,
				TurnID:         notification.Turn.ID,
				Status:         notification.Turn.Status,
				Error:          notification.Turn.Error,
			},
			items: notification.Turn.Items,
		}, nil

	default:
		return parsedServerEvent{}, errUnsupportedServerEvent
	}
}

func validateEventIDs(threadID, turnID string) error {
	if threadID == "" {
		return errors.New("app-server event has no thread id")
	}
	if turnID == "" {
		return errors.New("app-server event has no turn id")
	}
	return nil
}

func validateThreadItem(item ThreadItem) error {
	if item.ID == "" {
		return errors.New("app-server event item has no id")
	}
	if item.Type == "" {
		return errors.New("app-server event item has no type")
	}
	return nil
}

func validateTokenUsage(usage TokenUsage) error {
	if usage.ModelContextWindow != nil && *usage.ModelContextWindow < 0 {
		return errors.New("app-server token usage has a negative context window")
	}
	if negativeTokenCount(usage.Last) || negativeTokenCount(usage.Total) {
		return errors.New("app-server token usage has a negative count")
	}
	return nil
}

func negativeTokenCount(usage TokenUsageBreakdown) bool {
	return usage.CachedInputTokens < 0 || usage.InputTokens < 0 ||
		usage.OutputTokens < 0 || usage.ReasoningOutputTokens < 0 ||
		usage.TotalTokens < 0
}
