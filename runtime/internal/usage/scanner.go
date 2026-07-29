package usage

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type parserState struct {
	SessionID     string `json:"sessionId,omitempty"`
	ForkSessionID string `json:"forkSessionId,omitempty"`
	ForkParentID  string `json:"forkParentId,omitempty"`
	TurnID        string `json:"turnId,omitempty"`
	Model         string `json:"model,omitempty"`
	Previous      Tokens `json:"previous,omitempty"`
	Fast          bool   `json:"fast,omitempty"`
	IndexVersion  int    `json:"indexVersion,omitempty"`
}

// Increment when parser or pricing semantics change so already-indexed events
// are rebuilt from their source logs on the next sync.
const usageIndexVersion = 6

type entry struct {
	key, source, provider, sessionID, model string
	replayKey                               string
	offset, timestampMS                     int64
	tokens                                  Tokens
	recorded                                *float64
	calculated                              float64
	pricingFound                            bool
}

func (s *Service) Sync(ctx context.Context) (ScanStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.database(ctx)
	if err != nil {
		return ScanStats{}, err
	}
	stats := ScanStats{}
	for provider, roots := range s.providerRoots() {
		for _, root := range roots {
			fast := provider == "codex" && codexFastMode(filepath.Dir(root))
			err := filepath.WalkDir(root, func(path string, item os.DirEntry, walkErr error) error {
				if walkErr != nil || item.IsDir() || !strings.HasSuffix(strings.ToLower(item.Name()), ".jsonl") {
					return nil
				}
				stats.FilesSeen++
				return s.syncFile(ctx, db, provider, path, fast, &stats)
			})
			if err != nil && !os.IsNotExist(err) {
				return stats, err
			}
		}
	}
	return stats, nil
}

func codexFastMode(configDir string) bool {
	encoded, err := os.ReadFile(filepath.Join(configDir, "config.toml"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(encoded), "\n") {
		setting := strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		key, value, found := strings.Cut(setting, "=")
		if !found || strings.TrimSpace(key) != "service_tier" {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		return value == "fast" || value == "priority"
	}
	return false
}

func (s *Service) providerRoots() map[string][]string {
	roots := map[string][]string{
		"claude": append([]string(nil), s.options.ClaudeRoots...),
		"codex":  append([]string(nil), s.options.CodexRoots...),
	}
	entries, err := os.ReadDir(s.options.RunnerStateDir)
	if err == nil {
		for _, item := range entries {
			if item.IsDir() || !strings.HasSuffix(item.Name(), ".json") {
				continue
			}
			metadata, err := state.ReadRunnerMetadata(filepath.Join(s.options.RunnerStateDir, item.Name()))
			if err != nil || metadata.ConfigDir == "" {
				continue
			}
			switch state.CommandTool(metadata.Info.Cmd) {
			case state.ToolClaude:
				roots["claude"] = append(roots["claude"], filepath.Join(metadata.ConfigDir, "projects"))
			case state.ToolCodex:
				roots["codex"] = append(roots["codex"], filepath.Join(metadata.ConfigDir, "sessions"))
			}
		}
	}
	for provider, candidates := range roots {
		seen := make(map[string]struct{}, len(candidates))
		unique := candidates[:0]
		for _, candidate := range candidates {
			cleaned := filepath.Clean(strings.TrimSpace(candidate))
			if cleaned == "." || cleaned == "" {
				continue
			}
			if _, exists := seen[cleaned]; exists {
				continue
			}
			seen[cleaned] = struct{}{}
			unique = append(unique, cleaned)
		}
		roots[provider] = unique
	}
	return roots
}

func (s *Service) syncFile(ctx context.Context, db *sql.DB, provider, path string, fast bool, stats *ScanStats) error {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	var offset, oldSize, oldMtime int64
	var encodedState string
	err = db.QueryRowContext(ctx, `SELECT offset_bytes, size_bytes, mtime_ns, parser_state FROM usage_sources WHERE path = ?`, path).Scan(&offset, &oldSize, &oldMtime, &encodedState)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	knownSource := err == nil
	state := parserState{}
	_ = json.Unmarshal([]byte(encodedState), &state)
	rewrittenInPlace := knownSource && info.Size() == oldSize && info.ModTime().UnixNano() != oldMtime
	pricingModeChanged := provider == "codex" && knownSource && state.Fast != fast
	indexChanged := knownSource && state.IndexVersion != usageIndexVersion
	if info.Size() < oldSize || offset > info.Size() || rewrittenInPlace || pricingModeChanged || indexChanged {
		if _, err := db.ExecContext(ctx, `DELETE FROM usage_entries WHERE source_path = ?`, path); err != nil {
			return err
		}
		offset, oldSize, state = 0, 0, parserState{}
	}
	if knownSource && info.Size() == oldSize && !rewrittenInPlace && !pricingModeChanged && !indexChanged {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	stats.FilesRead++
	reader := bufio.NewReaderSize(file, 128*1024)
	currentOffset := offset
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr == io.EOF && len(line) > 0 {
			break // leave an incomplete append for the next sync
		}
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		if len(line) == 0 {
			break
		}
		lineOffset := currentOffset
		currentOffset += int64(len(line))
		stats.LinesRead++
		var parsed *entry
		if provider == "claude" {
			parsed = parseClaudeLine(path, lineOffset, bytes.TrimSpace(line), info.ModTime())
		} else {
			parsed = parseCodexLine(path, lineOffset, bytes.TrimSpace(line), info.ModTime(), &state, fast)
		}
		if parsed != nil {
			if err := upsertEntry(ctx, db, *parsed); err != nil {
				return err
			}
			stats.EntriesSeen++
		}
		if readErr == io.EOF {
			break
		}
	}
	state.Fast = fast
	state.IndexVersion = usageIndexVersion
	stateJSON, _ := json.Marshal(state)
	_, err = db.ExecContext(ctx, `INSERT INTO usage_sources(path, provider, offset_bytes, size_bytes, mtime_ns, parser_state)
VALUES(?, ?, ?, ?, ?, ?)
ON CONFLICT(path) DO UPDATE SET provider=excluded.provider, offset_bytes=excluded.offset_bytes,
size_bytes=excluded.size_bytes, mtime_ns=excluded.mtime_ns, parser_state=excluded.parser_state`,
		path, provider, currentOffset, info.Size(), info.ModTime().UnixNano(), string(stateJSON))
	return err
}

func upsertEntry(ctx context.Context, db *sql.DB, value entry) error {
	if value.replayKey != "" {
		var replayed bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM usage_entries WHERE event_key = ?)`, value.replayKey).Scan(&replayed); err != nil {
			return err
		}
		if replayed {
			return nil
		}
	}
	_, err := db.ExecContext(ctx, `INSERT INTO usage_entries(
event_key, source_path, source_offset, provider, provider_session_id, timestamp_ms, model,
input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
reasoning_tokens, recorded_cost_usd, calculated_cost_usd, pricing_found)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(event_key) DO UPDATE SET
source_path=CASE
  WHEN usage_entries.source_path = excluded.source_path OR usage_entries.source_path LIKE 'live://%'
    THEN excluded.source_path
  ELSE usage_entries.source_path
END,
source_offset=CASE
  WHEN usage_entries.source_path = excluded.source_path OR usage_entries.source_path LIKE 'live://%'
    THEN excluded.source_offset
  ELSE usage_entries.source_offset
END,
timestamp_ms=CASE
  WHEN usage_entries.source_path = excluded.source_path OR usage_entries.source_path LIKE 'live://%'
    THEN excluded.timestamp_ms
  ELSE usage_entries.timestamp_ms
END,
model=CASE WHEN excluded.model != '' THEN excluded.model ELSE usage_entries.model END,
input_tokens=CASE WHEN
  (excluded.input_tokens + excluded.output_tokens + excluded.cache_creation_tokens + excluded.cache_read_tokens) >
  (usage_entries.input_tokens + usage_entries.output_tokens + usage_entries.cache_creation_tokens + usage_entries.cache_read_tokens)
  THEN excluded.input_tokens ELSE usage_entries.input_tokens END,
output_tokens=CASE WHEN
  (excluded.input_tokens + excluded.output_tokens + excluded.cache_creation_tokens + excluded.cache_read_tokens) >
  (usage_entries.input_tokens + usage_entries.output_tokens + usage_entries.cache_creation_tokens + usage_entries.cache_read_tokens)
  THEN excluded.output_tokens ELSE usage_entries.output_tokens END,
cache_creation_tokens=CASE WHEN
  (excluded.input_tokens + excluded.output_tokens + excluded.cache_creation_tokens + excluded.cache_read_tokens) >
  (usage_entries.input_tokens + usage_entries.output_tokens + usage_entries.cache_creation_tokens + usage_entries.cache_read_tokens)
  THEN excluded.cache_creation_tokens ELSE usage_entries.cache_creation_tokens END,
cache_read_tokens=CASE WHEN
  (excluded.input_tokens + excluded.output_tokens + excluded.cache_creation_tokens + excluded.cache_read_tokens) >
  (usage_entries.input_tokens + usage_entries.output_tokens + usage_entries.cache_creation_tokens + usage_entries.cache_read_tokens)
  THEN excluded.cache_read_tokens ELSE usage_entries.cache_read_tokens END,
reasoning_tokens=MAX(usage_entries.reasoning_tokens, excluded.reasoning_tokens),
recorded_cost_usd=CASE
  WHEN excluded.recorded_cost_usd IS NOT NULL THEN excluded.recorded_cost_usd
  ELSE usage_entries.recorded_cost_usd
END,
calculated_cost_usd=CASE WHEN
  (excluded.input_tokens + excluded.output_tokens + excluded.cache_creation_tokens + excluded.cache_read_tokens) >
  (usage_entries.input_tokens + usage_entries.output_tokens + usage_entries.cache_creation_tokens + usage_entries.cache_read_tokens)
  THEN excluded.calculated_cost_usd ELSE usage_entries.calculated_cost_usd END,
pricing_found=MAX(usage_entries.pricing_found, excluded.pricing_found)
WHERE
  (excluded.input_tokens + excluded.output_tokens + excluded.cache_creation_tokens + excluded.cache_read_tokens) >
  (usage_entries.input_tokens + usage_entries.output_tokens + usage_entries.cache_creation_tokens + usage_entries.cache_read_tokens)
  OR excluded.reasoning_tokens > usage_entries.reasoning_tokens
  OR (usage_entries.source_path LIKE 'live://%' AND excluded.source_path NOT LIKE 'live://%')
  OR (usage_entries.model = '' AND excluded.model != '')
  OR (usage_entries.recorded_cost_usd IS NULL AND excluded.recorded_cost_usd IS NOT NULL)`,
		value.key, value.source, value.offset, value.provider, value.sessionID, value.timestampMS, value.model,
		value.tokens.Input, value.tokens.Output, value.tokens.CacheCreation, value.tokens.CacheRead, value.tokens.Reasoning,
		value.recorded, value.calculated, value.pricingFound)
	return err
}

func parseClaudeLine(path string, offset int64, raw []byte, fallback time.Time) *entry {
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	if data, ok := object(value["data"]); ok {
		if nested, ok := object(data["message"]); ok {
			value = nested
		}
	}
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
		sessionID = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	model := text(message, "model")
	stamp := parseTimestamp(text(value, "timestamp"), fallback)
	messageID := text(message, "id")
	key := liveClaudeKey(sessionID, messageID)
	if messageID == "" {
		key = fmt.Sprintf("claude:%s:%d", path, offset)
	}
	var recorded *float64
	if cost, ok := number(value["costUSD"]); ok {
		recorded = &cost
	}
	calculated, found := price(model, tokens, false)
	return &entry{key: key, source: path, provider: "claude", sessionID: sessionID, model: model,
		offset: offset, timestampMS: stamp.UnixMilli(), tokens: tokens, recorded: recorded,
		calculated: calculated, pricingFound: found}
}

func parseCodexLine(path string, offset int64, raw []byte, fallback time.Time, state *parserState, fast bool) *entry {
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	payload, _ := object(value["payload"])
	typeName := text(value, "type")
	payloadType := text(payload, "type")
	if typeName == "session_meta" {
		candidate := text(payload, "id", "session_id")
		if state.SessionID == "" {
			state.SessionID = candidate
			if parent := codexForkParent(payload); parent != "" {
				state.ForkSessionID = candidate
				state.ForkParentID = parent
			}
		} else if state.ForkSessionID == "" {
			state.SessionID = candidate
		}
		return nil
	}
	if typeName == "event_msg" && payloadType == "task_started" {
		state.TurnID = text(payload, "turn_id", "turnId")
		state.Previous = Tokens{}
		return nil
	}
	if typeName == "turn_context" {
		state.TurnID = text(payload, "turn_id", "turnId")
		if model := text(payload, "model"); model != "" {
			state.Model = model
		}
		return nil
	}
	if typeName != "event_msg" || payloadType != "token_count" {
		return nil
	}
	info, _ := object(payload["info"])
	if model := text(info, "model"); model != "" {
		state.Model = model
	}
	usage, hasLast := object(info["last_token_usage"])
	if !hasLast {
		usage, hasLast = object(info["lastTokenUsage"])
	}
	total, hasTotal := object(info["total_token_usage"])
	if !hasTotal {
		total, hasTotal = object(info["totalTokenUsage"])
	}
	var tokens Tokens
	if hasLast {
		tokens = codexTokens(usage)
	} else if hasTotal {
		current := codexTokens(total)
		tokens = subtractTokens(current, state.Previous)
	}
	if hasTotal {
		state.Previous = codexTokens(total)
	}
	if tokens.Total() == 0 {
		return nil
	}
	sessionID := state.SessionID
	if sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	stamp := parseTimestamp(text(value, "timestamp"), fallback)
	calculated, found := price(state.Model, tokens, fast)
	key := fmt.Sprintf("codex:%s:%d", path, offset)
	replayKey := ""
	if state.TurnID != "" {
		key = liveCodexKey(sessionID, state.TurnID)
		if state.ForkParentID != "" {
			replayKey = liveCodexKey(state.ForkParentID, state.TurnID)
		}
	}
	return &entry{key: key, source: path, provider: "codex",
		sessionID: sessionID, model: state.Model, offset: offset, timestampMS: stamp.UnixMilli(),
		tokens: tokens, calculated: calculated, pricingFound: found, replayKey: replayKey}
}

func codexForkParent(payload map[string]any) string {
	if parent := text(payload, "forked_from_id", "parent_thread_id", "session_id"); parent != "" && parent != text(payload, "id") {
		return parent
	}
	source, ok := object(payload["source"])
	if !ok {
		return ""
	}
	subagent, ok := object(source["subagent"])
	if !ok {
		return ""
	}
	spawn, ok := object(subagent["thread_spawn"])
	if !ok {
		return ""
	}
	return text(spawn, "parent_thread_id")
}

func codexTokens(value map[string]any) Tokens {
	input := integer(value, "input_tokens", "inputTokens", "prompt_tokens", "promptTokens")
	cached := integer(value, "cached_input_tokens", "cachedInputTokens")
	return Tokens{
		Input: max(0, input-cached), Output: integer(value, "output_tokens", "outputTokens", "completion_tokens", "completionTokens"),
		CacheRead: cached, Reasoning: integer(value, "reasoning_output_tokens", "reasoningOutputTokens"),
	}
}

func subtractTokens(current, previous Tokens) Tokens {
	return Tokens{Input: max(0, current.Input-previous.Input), Output: max(0, current.Output-previous.Output),
		CacheCreation: max(0, current.CacheCreation-previous.CacheCreation), CacheRead: max(0, current.CacheRead-previous.CacheRead),
		Reasoning: max(0, current.Reasoning-previous.Reasoning)}
}

func object(value any) (map[string]any, bool) {
	result, ok := value.(map[string]any)
	return result, ok
}
func text(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if result, ok := value[key].(string); ok {
			return result
		}
	}
	return ""
}
func integer(value map[string]any, keys ...string) int64 {
	for _, key := range keys {
		if result, ok := number(value[key]); ok {
			return int64(result)
		}
	}
	return 0
}
func number(value any) (float64, bool) { result, ok := value.(float64); return result, ok }
func parseTimestamp(value string, fallback time.Time) time.Time {
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed
	}
	return fallback
}
