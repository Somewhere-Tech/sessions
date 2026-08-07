package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

// End to end through the real transcript watcher: a Claude conversation that
// Claude titles "TexasT" must stop being called "Claude - Projects" in
// Sessions. Nothing here is faked but the runner -- the transcript is a real
// Claude JSONL under a real project directory, the watcher tails it, and the
// name is read back from the metadata file on disk.
func TestALiveClaudeConversationTakesTheTitleClaudeGivesIt(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "project")
	conversation := "aaaaaaaa-1111-2222-3333-444444444444"
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	config := state.Config{
		DefaultShell: "/bin/bash", DefaultCwd: cwd, DefaultCols: 300, DefaultRows: 50,
		StateRoot: filepath.Join(root, "state"), RunnerStateDir: filepath.Join(root, "state", "runners"),
		UserStateRoot: filepath.Join(root, "user"), LaunchAgentsDir: filepath.Join(root, "agents"),
	}
	manager := NewManager(config, prototest.NewLauncher())
	defer manager.Close()
	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "claude", Args: []string{"--session-id", conversation}, Cwd: cwd,
		Profile: "texas", Name: "Claude - Projects",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Name != "Claude - Projects" {
		t.Fatalf("created name = %q, want the launch auto-name", created.Name)
	}

	// The conversation Claude is keeping for this session, in the place and
	// format Claude keeps it in.
	projectDir := filepath.Join(created.ConfigDir, "projects", watch.EncodeClaudeCWD(cwd))
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(projectDir, conversation+".jsonl")
	appendRecord(t, transcript, `{"type":"user","uuid":"one","timestamp":"2026-08-05T10:00:00Z","message":{"role":"user","content":"how do texas rate tables work"}}`)

	// Claude titles the conversation, exactly as it does in a real transcript.
	appendRecord(t, transcript, `{"type":"ai-title","aiTitle":"TexasT","sessionId":"`+conversation+`"}`)
	waitForName(t, manager, created.ID, "TexasT")
	if name, source := persistedName(t, config, created.ID); name != "TexasT" || source != state.NameSourceProvider {
		t.Fatalf("persisted name/source = %q/%q, want %q/%q", name, source, "TexasT", state.NameSourceProvider)
	}

	// A title set inside Claude outranks the generated one, and the session
	// keeps following.
	appendRecord(t, transcript, `{"type":"custom-title","customTitle":"Texas rate tables","sessionId":"`+conversation+`"}`)
	waitForName(t, manager, created.ID, "Texas rate tables")

	// From the rename on, the card is the user's and Claude retitling its own
	// conversation no longer touches it.
	if _, err := manager.UpdateName(context.Background(), created.ID, "Texas billing"); err != nil {
		t.Fatal(err)
	}
	appendRecord(t, transcript, `{"type":"custom-title","customTitle":"Something else entirely","sessionId":"`+conversation+`"}`)
	assertNameHolds(t, manager, created.ID, "Texas billing")

	// --auto hands it back, and the title Claude already has applies at once.
	name, err := manager.ReleaseName(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if name != "Something else entirely" {
		t.Fatalf("rename --auto returned %q, want the provider's current title %q", name, "Something else entirely")
	}
}

// A structured Claude session has no transcript watcher; its records arrive
// from the runner as Structured frames, which the runner client decodes as
// EventCodex whichever provider produced them. The title has to be followed on
// that path too or Rich Claude sessions are the one kind that never gets one.
func TestAStructuredClaudeSessionAlsoTakesItsProviderTitle(t *testing.T) {
	root := t.TempDir()
	config := state.Config{
		DefaultShell: "/bin/bash", DefaultCwd: root, DefaultCols: 300, DefaultRows: 50,
		StateRoot: filepath.Join(root, "state"), RunnerStateDir: filepath.Join(root, "state", "runners"),
		LaunchAgentsDir: filepath.Join(root, "agents"),
	}
	launcher := prototest.NewLauncher()
	manager := NewManager(config, launcher, ManagerOptions{DisableWatchers: true})
	defer manager.Close()
	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "claude", Cwd: root, Kind: state.KindClaudeStructured, Name: "Claude - Projects",
		Args: []string{"--session-id", "aaaaaaaa-1111-2222-3333-444444444444"},
	})
	if err != nil {
		t.Fatal(err)
	}
	launcher.Runner(created.ID).AddCodexEvent(map[string]any{"type": "ai-title", "aiTitle": "TexasT"})
	waitForName(t, manager, created.ID, "TexasT")
}

func appendRecord(t *testing.T, path, record string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(record + "\n"); err != nil {
		t.Fatal(err)
	}
}

func sessionName(t *testing.T, manager *Manager, id string) string {
	t.Helper()
	session, ok := manager.Get(id)
	if !ok {
		t.Fatalf("session %s is not live", id)
	}
	return session.Info().Name
}

func waitForName(t *testing.T, manager *Manager, id, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if sessionName(t, manager, id) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session name = %q, want %q — the provider titled the conversation and Sessions kept its own name",
		sessionName(t, manager, id), want)
}

func assertNameHolds(t *testing.T, manager *Manager, id, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := sessionName(t, manager, id); got != want {
			t.Fatalf("session name = %q, want %q — a provider title overwrote the user's rename", got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func persistedName(t *testing.T, config state.Config, id string) (string, string) {
	t.Helper()
	metadata, err := state.ReadRunnerMetadata(filepath.Join(config.RunnerStateDir, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return metadata.Name, metadata.NameSource
}
