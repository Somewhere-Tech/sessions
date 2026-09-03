package integrations

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ansi"
	"github.com/somewhere-tech/sessions/runtime/internal/backup"
	"github.com/somewhere-tech/sessions/runtime/internal/providerargs"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

func (h *HistoryStore) providerConversations() []watch.ResumableSession {
	h.providerMu.Lock()
	defer h.providerMu.Unlock()
	now := time.Now()
	if !h.providerCachedAt.IsZero() && now.Sub(h.providerCachedAt) < 2*time.Second {
		return append([]watch.ResumableSession(nil), h.providerCache...)
	}
	h.providerCache = watch.ScanResumableConversationsIn(h.options.ClaudeProjectsDir, h.options.CodexSessionsDir)
	h.providerCachedAt = now
	return append([]watch.ResumableSession(nil), h.providerCache...)
}

// ResumableProviderConversations returns the same short-lived provider scan
// used by History. Resume surfaces call this after History so one request does
// not recursively scan every Claude and Codex file twice.
func (h *HistoryStore) ResumableProviderConversations() []watch.ResumableSession {
	return h.providerConversations()
}

func (h *HistoryStore) archivedClaudeConversations() []watch.ArchivedClaudeConversation {
	h.providerMu.Lock()
	defer h.providerMu.Unlock()
	now := time.Now()
	if !h.archiveCachedAt.IsZero() && now.Sub(h.archiveCachedAt) < 2*time.Second {
		return append([]watch.ArchivedClaudeConversation(nil), h.archiveCache...)
	}
	h.archiveCache = watch.ScanArchivedClaudeConversations(h.options.ClaudeHistoryPath)
	resumable := make(map[string]bool, len(h.providerCache))
	for _, source := range h.providerCache {
		if source.Tool == "claude" {
			resumable[source.SessionID] = true
		}
	}
	filtered := h.archiveCache[:0]
	for _, source := range h.archiveCache {
		if !resumable[source.SessionID] {
			filtered = append(filtered, source)
		}
	}
	h.archiveCache = filtered
	h.archiveCachedAt = now
	return append([]watch.ArchivedClaudeConversation(nil), h.archiveCache...)
}

func historyProviderKey(tool, providerID string) string {
	return strings.TrimSpace(tool) + ":" + strings.TrimSpace(providerID)
}

func externalHistoryID(tool, providerID string) string {
	return "provider:" + historyProviderKey(tool, providerID)
}

func archivedHistoryID(providerID string) string {
	return "provider-history:claude:" + strings.TrimSpace(providerID)
}

func archivedSourceFingerprint(source watch.ArchivedClaudeConversation) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%s\x00%d\x00%d", source.SessionID, source.ModifiedAt, len(source.Prompts))
	for _, prompt := range source.Prompts {
		_, _ = fmt.Fprintf(hash, "\x00%d\x00%s", prompt.Timestamp, prompt.Text)
	}
	return fmt.Sprintf("%x", hash.Sum(nil)[:16])
}

func (h *HistoryStore) archivedTranscript(source watch.ArchivedClaudeConversation) TranscriptResponse {
	session := h.describeArchived(source)
	messages := make([]TranscriptMessage, 0, len(source.Prompts))
	for index, prompt := range source.Prompts {
		timestamp := time.UnixMilli(prompt.Timestamp).UTC().Format(time.RFC3339Nano)
		messages = append(messages, TranscriptMessage{
			Index: index, ID: fmt.Sprintf("prompt-history:%s:%d", source.SessionID, index),
			Role: "user", Text: prompt.Text, Timestamp: &timestamp,
		})
	}
	return TranscriptResponse{SchemaVersion: SchemaVersion, Session: session, Messages: messages}
}

func (h *HistoryStore) archivedTranscriptWindow(source watch.ArchivedClaudeConversation, options TranscriptWindowOptions) (TranscriptResponse, error) {
	response := h.archivedTranscript(source)
	response.Session.MessageCount = len(response.Messages)
	response.Session.MessageCountUncounted = false
	end := options.End
	if end < 0 || end > len(response.Messages) {
		end = len(response.Messages)
	}
	start := min(max(0, options.Start), end)
	response.Messages = response.Messages[start:end]
	response.Truncated = len(response.Messages) != response.Session.MessageCount
	response.NextIndex = end
	response.HasMore = end < response.Session.MessageCount
	if options.ExpectedMessage != "" {
		matched := options.ExpectedIndex >= 0 && options.ExpectedIndex < len(source.Prompts) &&
			fmt.Sprintf("prompt-history:%s:%d", source.SessionID, options.ExpectedIndex) == options.ExpectedMessage
		if !matched {
			return TranscriptResponse{}, ErrHistoryChanged
		}
	}
	return response, nil
}

// historyDisplayName names a conversation in history. A Sessions name that is
// not a launch-time auto-name outranks a title Claude generated, but never a
// title set inside Claude itself.
//
// The provider's own title and its shaping come from
// state.ProviderConversationTitle, which is the same derivation the daemon
// adopts into the live session's name, so a conversation cannot be called one
// thing on the session card and another in history.
func historyDisplayName(source backup.Session) string {
	if !genericHistoryName(source.Name) {
		if custom := compactHistoryTitle(source.ClaudeCustomTitle); custom != "" {
			return custom
		}
		if name := compactHistoryTitle(source.Name); name != "" {
			return name
		}
	}
	if title := state.ProviderConversationTitle(source.ClaudeCustomTitle, source.ClaudeAITitle); title != "" {
		return title
	}
	for _, candidate := range []string{source.Description, source.Name} {
		if value := compactHistoryTitle(candidate); value != "" {
			return value
		}
	}
	return source.ID
}

func genericHistoryName(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, prefix := range []string{"claude - ", "claude — ", "codex - ", "codex — ", "shell - ", "shell — "} {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

// compactHistoryTitle is the shared conversation-title shaping. It lives in
// state because the daemon applies the same shaping when it adopts a provider
// title as a session name.
func compactHistoryTitle(value string) string {
	return state.CompactConversationTitle(value)
}

func providerSessionID(source backup.Session, tool string) string {
	switch tool {
	case "claude":
		if value := strings.TrimSpace(source.ClaudeSessionID); value != "" {
			return value
		}
		return extractHistoryClaudeSessionID(source.Args)
	case "codex":
		if value := strings.TrimSpace(source.ConversationID); value != "" {
			return value
		}
		return extractCodexConversationID(source.Args)
	default:
		return ""
	}
}

// These two used to be narrower than every other reader of the same argv: the
// Claude one missed `-r <uuid>` and the joined `--session-id=<uuid>` form, and
// the Codex one saw only the bare `resume` subcommand, so a conversation
// reopened with `codex --resume <uuid>` had no identity in history.
func extractHistoryClaudeSessionID(args []string) string {
	return providerargs.ClaudeSessionID(args)
}

func extractCodexConversationID(args []string) string {
	return providerargs.CodexConversationID(args)
}

func isProviderUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}

func historySourceFingerprint(path string, info os.FileInfo) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d", path, info.Size(), info.ModTime().UnixNano())))
	return fmt.Sprintf("%x", sum[:16])
}

// messageCount reports the normalized message count of one transcript and how
// many torn records were skipped to get it. Counting still parses the file —
// a message count is not derivable from line counts — but it never retains the
// messages, so listing a multi-gigabyte history costs constant memory instead
// of a full transcript per session.
func (h *HistoryStore) messageCount(path, tool string, info os.FileInfo) (int, int, error) {
	h.cacheMu.Lock()
	cached, ok := h.cache[path]
	if ok && cached.size == info.Size() && cached.modTimeNano == info.ModTime().UnixNano() {
		h.cacheClock++
		cached.used = h.cacheClock
		h.cache[path] = cached
		h.cacheMu.Unlock()
		return cached.count, cached.skipped, nil
	}
	h.cacheMu.Unlock()
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	// discardEveryMessage keeps the streaming normalizer's counting and
	// skip-accounting while retaining none of the messages it builds.
	discardEveryMessage := func(TranscriptMessage) bool { return false }
	_, count, skipped, err := normalizeTranscriptReaderSelected(
		context.Background(), bufio.NewReader(file), path, tool, discardEveryMessage)
	closeErr := file.Close()
	if err != nil {
		return 0, 0, err
	}
	if closeErr != nil {
		return 0, 0, closeErr
	}
	h.cacheMu.Lock()
	h.cacheClock++
	h.cache[path] = historyCacheEntry{
		size: info.Size(), modTimeNano: info.ModTime().UnixNano(),
		count: count, skipped: skipped, used: h.cacheClock,
	}
	h.evictHistoryCacheLocked()
	h.cacheMu.Unlock()
	return count, skipped, nil
}

// evictHistoryCacheLocked drops the least recently used quarter once the cache
// passes its bound. Transcript paths are unbounded over a daemon's lifetime, so
// the cache must have a ceiling; dropping a quarter keeps the O(n) sweep rare.
func (h *HistoryStore) evictHistoryCacheLocked() {
	if len(h.cache) <= maxHistoryCacheEntries {
		return
	}
	cutoff := h.cacheClock - uint64(maxHistoryCacheEntries*3/4)
	for path, entry := range h.cache {
		if entry.used <= cutoff {
			delete(h.cache, path)
		}
	}
}

func historyTool(tool state.SessionTool, resolved string) string {
	if resolved != "" {
		return resolved
	}
	switch tool {
	case state.ToolClaude:
		return "claude"
	case state.ToolCodex:
		return "codex"
	case state.ToolTerminal:
		return "terminal"
	default:
		return string(tool)
	}
}

func normalizeTranscript(path, tool string, maxBytes int64) ([]TranscriptMessage, int, error) {
	return normalizeTranscriptContext(context.Background(), path, tool, maxBytes)
}

func normalizeTranscriptContext(ctx context.Context, path, tool string, maxBytes int64) ([]TranscriptMessage, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()

	var source io.Reader = file
	if maxBytes > 0 {
		source = io.LimitReader(file, maxBytes)
	}
	return normalizeTranscriptReaderContext(ctx, bufio.NewReader(source), path, tool)
}

func normalizeTranscriptWindow(path, tool string, options TranscriptWindowOptions) ([]TranscriptMessage, int, int, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, false, err
	}
	defer file.Close()

	matchedExpected := options.ExpectedMessage == ""
	messages, messageCount, skipped, err := normalizeTranscriptReaderSelected(context.Background(), bufio.NewReader(file), path, tool, func(message TranscriptMessage) bool {
		if message.Index == options.ExpectedIndex && message.ID == options.ExpectedMessage {
			matchedExpected = true
		}
		if message.Index < options.Start || (options.End >= 0 && message.Index >= options.End) {
			return false
		}
		return options.Role == "" || message.Role == options.Role
	})
	return messages, messageCount, skipped, matchedExpected, err
}

func normalizeTranscriptTail(path, tool string, maxBytes int64, maxMessages int) ([]TranscriptMessage, int, bool, error) {
	if maxBytes <= 0 || maxMessages <= 0 {
		return nil, 0, false, errors.New("preview limits must be positive: pass a positive byte and message budget")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, 0, false, err
	}
	offset := max(int64(0), info.Size()-maxBytes)
	truncated := offset > 0
	var reader *bufio.Reader
	if offset > 0 {
		// Keep a record that begins exactly at the window boundary. Otherwise
		// discard the partial first JSONL record before normalization.
		if _, err := file.Seek(offset-1, io.SeekStart); err != nil {
			return nil, 0, false, err
		}
		previous := []byte{0}
		if _, err := io.ReadFull(file, previous); err != nil {
			return nil, 0, false, err
		}
		reader = bufio.NewReader(io.LimitReader(file, maxBytes))
		if previous[0] != '\n' {
			if _, err := reader.ReadBytes('\n'); err != nil && !errors.Is(err, io.EOF) {
				return nil, 0, false, err
			}
		} else if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return nil, 0, false, err
		} else {
			reader = bufio.NewReader(io.LimitReader(file, maxBytes))
		}
	} else {
		reader = bufio.NewReader(io.LimitReader(file, maxBytes))
	}
	messages, skipped, err := normalizeTranscriptReader(reader, path, tool)
	if err != nil {
		return nil, 0, false, err
	}
	if len(messages) > maxMessages {
		messages = messages[len(messages)-maxMessages:]
		truncated = true
	}
	return messages, skipped, truncated, nil
}

func normalizeTranscriptReader(reader *bufio.Reader, path, tool string) ([]TranscriptMessage, int, error) {
	return normalizeTranscriptReaderContext(context.Background(), reader, path, tool)
}

func normalizeTranscriptReaderContext(ctx context.Context, reader *bufio.Reader, path, tool string) ([]TranscriptMessage, int, error) {
	messages, _, skipped, err := normalizeTranscriptReaderSelected(ctx, reader, path, tool, nil)
	return messages, skipped, err
}

// normalizeTranscriptReaderSelected streams one provider transcript. It follows
// the torn-record policy stated in runtime/internal/integrations/errors.go: a
// JSONL line it cannot decode is skipped, counted, and reported as the third
// result, so a truncated tail from a power cut costs exactly that record and
// the caller can still tell the read was degraded. include may be nil to keep
// every message, or return false for every message to count without retaining.
func normalizeTranscriptReaderSelected(
	ctx context.Context,
	reader *bufio.Reader,
	path, tool string,
	include func(TranscriptMessage) bool,
) ([]TranscriptMessage, int, int, error) {
	messages := make([]TranscriptMessage, 0)
	relayCalls := make(map[string]string)
	lineIndex := 0
	messageIndex := 0
	skipped := 0
	for {
		if lineIndex%256 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, 0, 0, err
			}
		}
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := strings.TrimSpace(string(line))
			currentIndex := lineIndex
			lineIndex++
			if trimmed != "" {
				var decoded map[string]any
				if json.Unmarshal([]byte(trimmed), &decoded) != nil {
					skipped++
				} else {
					if tool == "codex" {
						normalized := watch.NormalizeCodexRolloutLine(decoded, watch.CodexNormalizeContext{
							RolloutBasename: filepath.Base(path), LineIndex: currentIndex,
						})
						for _, event := range normalized.Events {
							for _, message := range transcriptMessages(event, relayCalls) {
								message.Index = messageIndex
								message.ID = transcriptMessageID(message)
								messageIndex++
								if include == nil || include(message) {
									messages = append(messages, message)
								}
							}
						}
					} else {
						for _, message := range transcriptMessages(decoded, relayCalls) {
							message.Index = messageIndex
							message.ID = transcriptMessageID(message)
							messageIndex++
							if include == nil || include(message) {
								messages = append(messages, message)
							}
						}
					}
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			// The file itself stopped being readable mid-stream. Unlike a torn
			// record this is not recoverable by skipping ahead, so the caller
			// is told rather than handed a silently short conversation; a
			// listing degrades that one row instead of dropping every session.
			return nil, 0, skipped, fmt.Errorf("read transcript %s: %w", path, readErr)
		}
	}
	return messages, messageIndex, skipped, nil
}

func transcriptMessageID(message TranscriptMessage) string {
	timestamp := ""
	if message.Timestamp != nil {
		timestamp = *message.Timestamp
	}
	sum := sha256.Sum256([]byte(message.Role + "\x00" + message.Kind + "\x00" + timestamp + "\x00" + message.Text))
	return fmt.Sprintf("%x", sum[:16])
}

func transcriptMessages(event map[string]any, relayCalls map[string]string) []TranscriptMessage {
	if event["type"] == "system" && event["subtype"] == "provider_fault" {
		detail, _ := event["detail"].(string)
		if strings.TrimSpace(detail) != "" {
			return []TranscriptMessage{{
				Role: "error", Kind: "provider_fault", Text: detail,
				Timestamp: normalizedTimestamp(event["timestamp"]),
			}}
		}
	}
	message, ok := event["message"].(map[string]any)
	if !ok {
		return nil
	}
	role, _ := message["role"].(string)
	if role != "user" && role != "assistant" {
		return nil
	}
	timestamp := normalizedTimestamp(event["timestamp"])
	result := make([]TranscriptMessage, 0, 2)
	if text := contentText(message["content"]); text != "" {
		text = stripTranscriptANSI(text)
		if !isHiddenProviderControlMessage(text) {
			messageRole := role
			if role == "user" && isSyntheticUserMessage(text) {
				messageRole = "tool"
			}
			kind := ""
			if messageRole == "tool" {
				kind = "automation"
			}
			result = append(result, TranscriptMessage{Role: messageRole, Kind: kind, Text: text, Timestamp: timestamp})
		}
	}
	blocks, _ := message["content"].([]any)
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "tool_use":
			name, _ := block["name"].(string)
			if !isRelayTool(name) {
				continue
			}
			id, _ := block["id"].(string)
			if id != "" {
				relayCalls[id] = name
			}
			if text := relayToolRequest(name, block["input"]); text != "" {
				result = append(result, TranscriptMessage{Role: "tool", Kind: relayToolKind(name), Text: text, Timestamp: timestamp})
			}
		case "tool_result":
			id, _ := block["tool_use_id"].(string)
			name := relayCalls[id]
			if name == "" {
				continue
			}
			if text := relayToolResult(name, block["content"]); text != "" {
				result = append(result, TranscriptMessage{Role: "tool", Kind: relayToolKind(name), Text: text, Timestamp: timestamp})
			}
		}
	}
	return result
}

func stripTranscriptANSI(text string) string {
	return strings.TrimSpace(ansi.Strip(text))
}

func isHiddenProviderControlMessage(text string) bool {
	trimmed := strings.TrimSpace(text)
	for _, prefix := range []string{
		"<command-name>", "<command-message>", "<command-args>", "<command-stdout>",
		"<local-command-", "<system-reminder>", "<recommended_plugins>", "<environment_context>",
		"# AGENTS.md instructions", "# CLAUDE.md instructions",
		"This session is being continued from a previous conversation",
		"Caveat: The messages below were generated by the user while",
	} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

func isSyntheticUserMessage(text string) bool {
	trimmed := strings.TrimSpace(text)
	for _, prefix := range []string{
		"<task-notification>", "<system-reminder>", "<local-command-", "<command-message>",
	} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	if strings.HasPrefix(trimmed, "[") {
		line := trimmed
		if end := strings.IndexByte(line, ']'); end >= 0 {
			line = strings.ToUpper(line[:end+1])
			return strings.Contains(line, " TICK") ||
				strings.Contains(line, " AUTOMATION") ||
				strings.Contains(line, " ROUTINE")
		}
	}
	return false
}

func contentText(content any) string {
	switch value := content.(type) {
	case string:
		return strings.TrimSpace(value)
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			block, ok := item.(map[string]any)
			if !ok || block["type"] != "text" {
				continue
			}
			if text, ok := block["text"].(string); ok && strings.TrimSpace(text) != "" {
				parts = append(parts, strings.TrimSpace(text))
			}
		}
		return strings.Join(parts, "\n\n")
	default:
		return ""
	}
}

var searchableRelayTools = map[string]struct{}{
	"agent":                  {},
	"spawn_agent":            {},
	"send_message":           {},
	"send_message_to_thread": {},
	"followup_task":          {},
	"create_thread":          {},
	"fork_thread":            {},
	"read_thread":            {},
	"wait_agent":             {},
	"wait_threads":           {},
	"handoff_thread":         {},
}

func isRelayTool(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for candidate := range searchableRelayTools {
		if name == candidate || strings.HasSuffix(name, "__"+candidate) || strings.HasSuffix(name, "."+candidate) {
			return true
		}
	}
	return false
}

func relayToolRequest(name string, input any) string {
	values, _ := input.(map[string]any)
	if len(values) == 0 {
		return ""
	}
	target := firstString(values, "target", "thread_id", "threadId", "recipient", "task_name", "subagent_type")
	body := firstString(values, "message", "prompt", "task", "objective", "description")
	if body == "" {
		return ""
	}
	label := relayToolLabel(name)
	if target != "" {
		label += " to " + target
	}
	return boundedRelayText(label+": "+body, 64<<10)
}

func relayToolKind(name string) string {
	switch relayToolLabel(name) {
	case "agent", "spawn_agent", "create_thread", "fork_thread":
		return "delegation"
	case "send_message", "send_message_to_thread", "followup_task", "handoff_thread":
		return "handoff"
	default:
		return "status"
	}
}

func relayToolResult(name string, content any) string {
	text := contentText(content)
	if text == "" {
		if value, ok := content.(string); ok {
			text = strings.TrimSpace(value)
		}
	}
	if text == "" {
		return ""
	}
	return boundedRelayText(relayToolLabel(name)+" result: "+text, 64<<10)
}

func relayToolLabel(name string) string {
	normalized := strings.ToLower(strings.TrimSpace(name))
	for candidate := range searchableRelayTools {
		if normalized == candidate || strings.HasSuffix(normalized, "__"+candidate) || strings.HasSuffix(normalized, "."+candidate) {
			return candidate
		}
	}
	return normalized
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func boundedRelayText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

func normalizedTimestamp(value any) *string {
	switch timestamp := value.(type) {
	case string:
		if timestamp == "" {
			return nil
		}
		return &timestamp
	case float64:
		var parsed time.Time
		if timestamp > -100_000_000_000 && timestamp < 100_000_000_000 {
			seconds := int64(timestamp)
			nanos := int64((timestamp - float64(seconds)) * float64(time.Second))
			parsed = time.Unix(seconds, nanos)
		} else {
			parsed = time.UnixMilli(int64(timestamp))
		}
		formatted := parsed.UTC().Format(time.RFC3339Nano)
		return &formatted
	default:
		return nil
	}
}
