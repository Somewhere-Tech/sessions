package delivery

import (
	"encoding/json"
	"errors"
	"strings"
)

const PasteStart = "\x1b[200~"
const PasteEnd = "\x1b[201~"

// MessageText removes exactly one legacy composer envelope, not embedded data.
func MessageText(text string) string {
	if strings.HasPrefix(text, PasteStart) && strings.HasSuffix(text, PasteEnd) {
		return strings.TrimSuffix(strings.TrimPrefix(text, PasteStart), PasteEnd)
	}
	return text
}

// LegacyPaste marks one message across PTY read boundaries. Terminal controls
// cannot be represented as literal text by this older protocol.
func LegacyPaste(data string) (string, error) {
	text := MessageText(data)
	for _, r := range text {
		if (r < 32 && r != '\n' && r != '\r' && r != '\t') || r == 127 {
			return "", errors.New("message not sent: legacy terminal messages cannot contain terminal control bytes; use a structured session for literal control text")
		}
	}
	return PasteStart + text + PasteEnd, nil
}

// NormalizedMessage permits only terminal newline and outer-whitespace
// normalization. Interior content, Unicode, and every line must match in full.
func NormalizedMessage(text string) string {
	return strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
}

// MatchingUserText accepts a complete fresh authored user event, never a
// timestamp, a suffix, a tool result, or the provider's working state.
func MatchingUserText(raw json.RawMessage, intended string) (string, bool) {
	var event struct {
		Type    string `json:"type"`
		Message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &event) != nil || event.Type != "user" || event.Message.Role != "user" {
		return "", false
	}
	var text string
	if json.Unmarshal(event.Message.Content, &text) != nil {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(event.Message.Content, &blocks) != nil {
			return "", false
		}
		var joined strings.Builder
		for _, block := range blocks {
			if block.Type != "text" && block.Type != "input_text" {
				return "", false
			}
			joined.WriteString(block.Text)
		}
		text = joined.String()
	}
	wanted := NormalizedMessage(MessageText(intended))
	return text, wanted != "" && NormalizedMessage(text) == wanted
}
