package watch

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type ArchivedClaudePrompt struct {
	Text      string
	Timestamp int64
}

type ArchivedClaudeConversation struct {
	SessionID        string
	Cwd              string
	ModifiedAt       int64
	FirstUserMessage string
	Prompts          []ArchivedClaudePrompt
}

// ScanArchivedClaudeConversations reads Claude's lightweight prompt index.
// It is a last-resort local recall surface for conversations whose full JSONL
// has already been removed by the provider. It contains user prompts only and
// must never be presented as a complete transcript. A separate authenticated
// recovery boundary may ask Claude to continue the exact recorded provider
// identity and workspace; this scanner itself never claims or launches that.
func ScanArchivedClaudeConversations(historyPath string) []ArchivedClaudeConversation {
	if strings.TrimSpace(historyPath) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return []ArchivedClaudeConversation{}
		}
		historyPath = filepath.Join(home, ".claude", "history.jsonl")
	}
	file, err := os.Open(historyPath)
	if err != nil {
		return []ArchivedClaudeConversation{}
	}
	defer file.Close()

	type historyEntry struct {
		Display   string `json:"display"`
		Project   string `json:"project"`
		SessionID string `json:"sessionId"`
		Timestamp int64  `json:"timestamp"`
	}
	byID := make(map[string]*ArchivedClaudeConversation)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var entry historyEntry
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
			continue
		}
		entry.SessionID = strings.TrimSpace(entry.SessionID)
		entry.Display = strings.TrimSpace(entry.Display)
		if entry.SessionID == "" || entry.Display == "" {
			continue
		}
		conversation := byID[entry.SessionID]
		if conversation == nil {
			conversation = &ArchivedClaudeConversation{
				SessionID: entry.SessionID, Cwd: strings.TrimSpace(entry.Project),
				FirstUserMessage: previewText(entry.Display),
			}
			byID[entry.SessionID] = conversation
		}
		if conversation.Cwd == "" {
			conversation.Cwd = strings.TrimSpace(entry.Project)
		}
		conversation.ModifiedAt = max(conversation.ModifiedAt, entry.Timestamp)
		conversation.Prompts = append(conversation.Prompts, ArchivedClaudePrompt{
			Text: entry.Display, Timestamp: entry.Timestamp,
		})
	}
	out := make([]ArchivedClaudeConversation, 0, len(byID))
	for _, conversation := range byID {
		sort.SliceStable(conversation.Prompts, func(i, j int) bool {
			return conversation.Prompts[i].Timestamp < conversation.Prompts[j].Timestamp
		})
		out = append(out, *conversation)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ModifiedAt != out[j].ModifiedAt {
			return out[i].ModifiedAt > out[j].ModifiedAt
		}
		return out[i].SessionID < out[j].SessionID
	})
	return out
}
