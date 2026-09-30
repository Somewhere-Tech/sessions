package state

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
)

func TestStructuredByteEvictionPreservesAbsoluteCursor(t *testing.T) {
	session := &Session{}
	raw, err := json.Marshal(map[string]string{"type": "assistant", "text": strings.Repeat("x", proto.MaxStructuredReplayBytes/2)})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 5; index++ {
		event := proto.Event{ClaudeEvent: raw}
		session.recordClaudeLocked(&event)
		if event.ClaudeIndex != int64(index) {
			t.Fatalf("event index = %d, want %d", event.ClaudeIndex, index)
		}
	}
	if len(session.claude) != 1 || session.claudeBase != 4 || session.claudeBase+int64(len(session.claude)) != 5 {
		t.Fatalf("window = base %d, count %d", session.claudeBase, len(session.claude))
	}
}
