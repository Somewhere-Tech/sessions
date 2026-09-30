package integrations

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

// Two conversations, one folder, one name. This is the arrangement that used to
// pair old history with an unrelated runtime, and the only thing that separates
// them is the provider's own conversation id: not the workspace they both ran
// in, and not the title they happen to share.
func TestTwoConversationsInOneWorkspaceResolveByProviderIdentityAlone(t *testing.T) {
	root := t.TempDir()
	claudeDir := filepath.Join(root, "claude-projects")
	cwd := filepath.Join(root, "project")
	cwdJSON, err := json.Marshal(cwd)
	if err != nil {
		t.Fatal(err)
	}
	const (
		firstUUID  = "11111111-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		secondUUID = "22222222-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	)
	// Same encoded project directory for both: the folder is shared, exactly as
	// two conversations opened in one repository share it.
	projectDir := filepath.Join(claudeDir, watch.EncodeClaudeCWD(cwd))
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(uuid, opening string) {
		t.Helper()
		contents := strings.Join([]string{
			`{"type":"user","cwd":` + string(cwdJSON) + `,"message":{"role":"user","content":"` + opening + `"}}`,
			`{"type":"assistant","message":{"role":"assistant","content":"Answer for ` + uuid + `"}}`,
			// Identical human titles: a chooser that matched on the title would
			// have nothing left to tell these apart.
			`{"type":"custom-title","customTitle":"Migration plan"}`,
		}, "\n") + "\n"
		if err := os.WriteFile(filepath.Join(projectDir, uuid+".jsonl"), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(firstUUID, "First conversation opening line")
	write(secondUUID, "Second conversation opening line")

	store := NewHistoryStore(HistoryOptions{
		RunnerStateDir: filepath.Join(root, "runners"), ClaudeProjectsDir: claudeDir,
		CodexSessionsDir: filepath.Join(root, "codex-sessions"),
		Machine:          "fixture-mac", DiscoverProviderHistory: true,
	})

	history, err := store.List(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Sessions) != 2 {
		t.Fatalf("history = %#v, want both conversations listed separately", history.Sessions)
	}
	byProvider := map[string]HistorySession{}
	for _, session := range history.Sessions {
		if session.CWD != cwd || session.Name != "Migration plan" {
			t.Fatalf("fixture assumption broken: %#v", session)
		}
		byProvider[session.ProviderSessionID] = session
	}
	if len(byProvider) != 2 {
		t.Fatalf("two conversations collapsed into %d identities: %#v", len(byProvider), history.Sessions)
	}

	for uuid, opening := range map[string]string{
		firstUUID:  "First conversation opening line",
		secondUUID: "Second conversation opening line",
	} {
		listed, ok := byProvider[uuid]
		if !ok {
			t.Fatalf("conversation %s is missing from history", uuid)
		}
		looked, err := store.Lookup(nil, listed.ID)
		if err != nil {
			t.Fatalf("Lookup(%s): %v", listed.ID, err)
		}
		if looked.ProviderSessionID != uuid || looked.ID != listed.ID {
			t.Fatalf("Lookup(%s) resolved %#v", listed.ID, looked)
		}
		transcript, err := store.Transcript(nil, listed.ID)
		if err != nil {
			t.Fatalf("Transcript(%s): %v", listed.ID, err)
		}
		if len(transcript.Messages) != 2 || transcript.Messages[0].Text != opening {
			t.Fatalf("Transcript(%s) returned the neighbour's conversation: %#v", listed.ID, transcript.Messages)
		}
		source, err := store.Source(nil, listed.ID)
		if err != nil {
			t.Fatalf("Source(%s): %v", listed.ID, err)
		}
		if filepath.Base(source.SourcePath) != uuid+".jsonl" {
			t.Fatalf("Source(%s) points at %q", listed.ID, source.SourcePath)
		}
	}

	// The workspace and the shared title are not identities. Neither resolves
	// anything, so neither can quietly stand in for the conversation that was
	// actually chosen.
	for _, notAnIdentity := range []string{cwd, "Migration plan", projectDir, firstUUID + " "} {
		if _, err := store.Lookup(nil, notAnIdentity); !errors.Is(err, ErrHistoryNotFound) {
			t.Fatalf("Lookup(%q) resolved a conversation; err=%v", notAnIdentity, err)
		}
	}
}
