package integrations

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

// ConversationDaySummary is a bounded local description of provider work.
// It is derived by streaming one JSONL file and retaining only a few short
// excerpts, never the transcript itself.
type ConversationDaySummary struct {
	CWD           string
	Origin        string
	FirstUser     string
	LastUser      string
	LastAssistant string
	FirstAt       int64
	LastAt        int64
	MessageCount  int
}

// SummarizeConversationDay reads a local Claude or Codex conversation and
// keeps only messages timestamped inside [start, end). Codex child rollouts
// retain only turns which the usage index attributed to the child, excluding
// its timestamp-rewritten parent snapshot.
func SummarizeConversationDay(path, tool string, start, end time.Time, observedTurnIDs []string) (ConversationDaySummary, error) {
	file, err := os.Open(path)
	if err != nil {
		return ConversationDaySummary{}, err
	}
	defer file.Close()

	result := ConversationDaySummary{}
	reader := bufio.NewReaderSize(file, 128*1024)
	lineIndex := 0
	fork := false
	currentTurnID := ""
	observedTurns := make(map[string]struct{}, len(observedTurnIDs))
	relayCalls := make(map[string]string)
	for _, turnID := range observedTurnIDs {
		observedTurns[turnID] = struct{}{}
	}
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			var decoded map[string]any
			if json.Unmarshal(line, &decoded) == nil {
				if tool == "codex" {
					if decoded["type"] == "session_meta" {
						if payload, ok := decoded["payload"].(map[string]any); ok {
							if result.CWD == "" {
								result.CWD, _ = payload["cwd"].(string)
							}
							if result.Origin == "" {
								result.Origin, _ = payload["originator"].(string)
							}
							if codexForkSource(payload) {
								fork = true
								result.Origin = "Codex child agent"
							}
						}
					}
					if turnID := codexLineTurn(decoded); turnID != "" {
						currentTurnID = turnID
					}
					if fork {
						if _, observed := observedTurns[currentTurnID]; !observed {
							lineIndex++
							if readErr != nil {
								break
							}
							continue
						}
					}
					normalized := watch.NormalizeCodexRolloutLine(decoded, watch.CodexNormalizeContext{
						RolloutBasename: filepath.Base(path), LineIndex: lineIndex,
					})
					for _, event := range normalized.Events {
						for _, message := range transcriptMessages(event, relayCalls) {
							mergeDayMessage(&result, message, start, end)
						}
					}
				} else {
					if cwd, ok := decoded["cwd"].(string); ok && strings.TrimSpace(cwd) != "" {
						result.CWD = cwd
					}
					for _, message := range transcriptMessages(decoded, relayCalls) {
						mergeDayMessage(&result, message, start, end)
					}
				}
			}
			lineIndex++
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return ConversationDaySummary{}, readErr
		}
	}
	if result.Origin == "" {
		if tool == "claude" {
			result.Origin = "Claude Code"
		} else {
			result.Origin = "Codex"
		}
	}
	return result, nil
}

func mergeDayMessage(summary *ConversationDaySummary, message TranscriptMessage, start, end time.Time) {
	if message.Timestamp == nil {
		return
	}
	stamp, err := time.Parse(time.RFC3339Nano, *message.Timestamp)
	if err != nil || stamp.Before(start) || !stamp.Before(end) {
		return
	}
	at := stamp.UnixMilli()
	text := compactExcerpt(message.Text, 600)
	if text == "" {
		return
	}
	if summary.FirstAt == 0 || at < summary.FirstAt {
		summary.FirstAt = at
	}
	if at > summary.LastAt {
		summary.LastAt = at
	}
	summary.MessageCount++
	if message.Role == "user" {
		if summary.FirstUser == "" {
			summary.FirstUser = compactExcerpt(message.Text, 240)
		}
		summary.LastUser = compactExcerpt(message.Text, 240)
	} else if message.Role == "assistant" {
		summary.LastAssistant = text
	}
}

func compactExcerpt(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > limit {
		return strings.TrimSpace(string(runes[:limit])) + "…"
	}
	return value
}

func codexForkSource(payload map[string]any) bool {
	source, ok := payload["source"].(map[string]any)
	if !ok {
		return false
	}
	subagent, ok := source["subagent"].(map[string]any)
	if !ok {
		return false
	}
	_, ok = subagent["thread_spawn"].(map[string]any)
	return ok
}

func codexLineTurn(decoded map[string]any) string {
	payload, ok := decoded["payload"].(map[string]any)
	if !ok {
		return ""
	}
	if decoded["type"] == "event_msg" && payload["type"] == "task_started" {
		turnID, _ := payload["turn_id"].(string)
		return turnID
	}
	if decoded["type"] == "turn_context" {
		turnID, _ := payload["turn_id"].(string)
		return turnID
	}
	if metadata, ok := payload["internal_chat_message_metadata_passthrough"].(map[string]any); ok {
		turnID, _ := metadata["turn_id"].(string)
		return turnID
	}
	return ""
}
