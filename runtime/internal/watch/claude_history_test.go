package watch

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanArchivedClaudeConversationsGroupsRetainedPrompts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	contents := "" +
		`{"display":"Build the BOLO app","project":"/work/bolo","sessionId":"bolo-id","timestamp":1000}` + "\n" +
		`{"display":"Fix its login","project":"/work/bolo","sessionId":"bolo-id","timestamp":3000}` + "\n" +
		`{"display":"Other work","project":"/work/other","sessionId":"other-id","timestamp":2000}` + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	conversations := ScanArchivedClaudeConversations(path)
	if len(conversations) != 2 {
		t.Fatalf("conversations = %#v", conversations)
	}
	got := conversations[0]
	if got.SessionID != "bolo-id" || got.Cwd != "/work/bolo" ||
		got.FirstUserMessage != "Build the BOLO app" || got.ModifiedAt != 3000 ||
		len(got.Prompts) != 2 || got.Prompts[1].Text != "Fix its login" {
		t.Fatalf("BOLO prompt history = %#v", got)
	}
}
