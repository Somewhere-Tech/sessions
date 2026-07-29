package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// RecordStructured writes one provider-native usage event as it crosses the
// running-session boundary. The later provider-log scan uses the same stable
// event keys, so backfill enriches or repairs this row instead of counting it
// a second time.
func (s *Service) RecordStructured(ctx context.Context, info state.SessionInfo, raw json.RawMessage) error {
	value := make(map[string]any)
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	var parsed *entry
	switch info.Tool {
	case state.ToolClaude:
		parsed = liveClaudeEntry(info, value)
	case state.ToolCodex:
		parsed = liveCodexEntry(info, value)
	}
	if parsed == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.database(ctx)
	if err != nil {
		return err
	}
	return upsertEntry(ctx, db, *parsed)
}

func liveClaudeEntry(info state.SessionInfo, value map[string]any) *entry {
	message, ok := object(value["message"])
	if !ok {
		return nil
	}
	usage, ok := object(message["usage"])
	if !ok {
		return nil
	}
	tokens := Tokens{
		Input: integer(usage, "input_tokens", "inputTokens"), Output: integer(usage, "output_tokens", "outputTokens"),
		CacheCreation: integer(usage, "cache_creation_input_tokens", "cacheCreationInputTokens"),
		CacheRead:     integer(usage, "cache_read_input_tokens", "cacheReadInputTokens"),
		Reasoning:     integer(usage, "reasoning_tokens", "reasoningTokens", "reasoning_output_tokens", "reasoningOutputTokens"),
	}
	if tokens.Total() == 0 {
		return nil
	}
	sessionID := text(value, "sessionId", "session_id")
	if sessionID == "" {
		sessionID = info.ClaudeSessionID
	}
	messageID := text(message, "id")
	if sessionID == "" || messageID == "" {
		return nil
	}
	model := text(message, "model")
	if model == "" {
		model = info.Model
	}
	var recorded *float64
	if cost, ok := firstNumber(value, "costUSD", "total_cost_usd", "totalCostUSD"); ok {
		recorded = &cost
	}
	calculated, found := price(model, tokens, false)
	return &entry{
		key: liveClaudeKey(sessionID, messageID), source: "live://" + info.ID, provider: "claude",
		sessionID: sessionID, model: model, timestampMS: parseTimestamp(text(value, "timestamp"), time.Now()).UnixMilli(),
		tokens: tokens, recorded: recorded, calculated: calculated, pricingFound: found,
	}
}

func liveCodexEntry(info state.SessionInfo, value map[string]any) *entry {
	if text(value, "source") != "codex-app-server" || text(value, "subtype") != "token_count" {
		return nil
	}
	usage, ok := object(value["usage"])
	if !ok {
		return nil
	}
	last, ok := object(usage["last"])
	if !ok {
		return nil
	}
	tokens := codexTokens(last)
	if tokens.Total() == 0 {
		return nil
	}
	sessionID := text(value, "conversationId", "conversation_id")
	if sessionID == "" {
		sessionID = info.ConversationID
	}
	turnID := text(value, "turnId", "turn_id")
	if sessionID == "" || turnID == "" {
		return nil
	}
	calculated, found := price(info.Model, tokens, info.Fast)
	return &entry{
		key: liveCodexKey(sessionID, turnID), source: "live://" + info.ID, provider: "codex",
		sessionID: sessionID, model: info.Model, timestampMS: parseTimestamp(text(value, "timestamp"), time.Now()).UnixMilli(),
		tokens: tokens, calculated: calculated, pricingFound: found,
	}
}

func liveClaudeKey(sessionID, messageID string) string {
	return fmt.Sprintf("claude:%s:%s", sessionID, messageID)
}

func liveCodexKey(sessionID, turnID string) string {
	return fmt.Sprintf("codex:%s:%s", sessionID, turnID)
}

func firstNumber(value map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		if result, ok := number(value[key]); ok {
			return result, true
		}
	}
	return 0, false
}
