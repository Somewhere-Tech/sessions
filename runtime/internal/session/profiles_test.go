package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

func TestProfileCreateInjectsPrivateToolHomesAndRecordsProvenance(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "ambient-claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "ambient-codex"))
	manager, launcher, store := newWorktreeTestManager(t, root)

	createdClaude, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "claude", Cwd: root, Name: "work claude", Profile: "work",
		Args: []string{"--session-id", "aaaaaaaa-1111-4222-8333-444444444444"},
	})
	if err != nil {
		t.Fatal(err)
	}
	claudeDir := filepath.Join(manager.config.UserStateRoot, "profiles", "claude", "work")
	if createdClaude.Profile != "work" || createdClaude.ConfigDir != claudeDir {
		t.Fatalf("Claude profile state = %#v", createdClaude)
	}
	assertMode(t, claudeDir, 0o700)
	claudeLaunch := launcher.Launches[len(launcher.Launches)-1]
	if claudeLaunch.Env["CLAUDE_CONFIG_DIR"] != claudeDir {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q, want %q", claudeLaunch.Env["CLAUDE_CONFIG_DIR"], claudeDir)
	}
	if _, present := claudeLaunch.Env["CODEX_HOME"]; present {
		t.Fatalf("profiled Claude launch leaked CODEX_HOME: %#v", claudeLaunch.Env)
	}

	createdCodex, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "codex", Cwd: root, Name: "personal codex", Profile: "personal",
	})
	if err != nil {
		t.Fatal(err)
	}
	codexDir := filepath.Join(manager.config.UserStateRoot, "profiles", "codex", "personal")
	codexLaunch := launcher.Launches[len(launcher.Launches)-1]
	if codexLaunch.Env["CODEX_HOME"] != codexDir {
		t.Fatalf("CODEX_HOME = %q, want %q", codexLaunch.Env["CODEX_HOME"], codexDir)
	}
	if _, present := codexLaunch.Env["CLAUDE_CONFIG_DIR"]; present {
		t.Fatalf("profiled Codex launch leaked CLAUDE_CONFIG_DIR: %#v", codexLaunch.Env)
	}
	assertMode(t, codexDir, 0o700)

	for _, request := range []state.CreateSessionRequest{
		{Cmd: "claude", Cwd: root, Env: map[string]string{"CLAUDE_CONFIG_DIR": "caller", "CODEX_HOME": "caller"}},
		{Cmd: "codex", Cwd: root, Env: map[string]string{"CLAUDE_CONFIG_DIR": "caller", "CODEX_HOME": "caller"}},
	} {
		if _, err := manager.Create(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		launch := launcher.Launches[len(launcher.Launches)-1]
		for _, key := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME"} {
			if _, present := launch.Env[key]; present {
				t.Fatalf("default %s launch set %s: %#v", request.Cmd, key, launch.Env)
			}
		}
	}

	metadata, err := state.ReadRunnerMetadata(filepath.Join(manager.config.RunnerStateDir, createdClaude.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Profile != "work" || metadata.ConfigDir != claudeDir {
		t.Fatalf("runner metadata profile = %#v", metadata)
	}
	events, err := store.Events(context.Background(), createdClaude.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("profile session has no ledger events")
	}
	var payload map[string]any
	if json.Unmarshal(events[0].Payload, &payload) != nil ||
		payload["profile"] != "work" || payload["config_dir"] != claudeDir {
		t.Fatalf("created profile payload = %s", events[0].Payload)
	}
	folded := ledger.Fold(events)
	if len(folded) != 1 || folded[0].Profile != "work" || folded[0].ConfigDir != claudeDir {
		t.Fatalf("folded profile state = %#v", folded)
	}

	profiles, err := manager.Profiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 || profiles[0].Tool != "claude" || profiles[0].Name != "work" ||
		profiles[0].Path != claudeDir || len(profiles[0].Sessions) != 1 || profiles[0].Sessions[0].ID != createdClaude.ID ||
		profiles[1].Tool != "codex" || profiles[1].Name != "personal" || profiles[1].Path != codexDir ||
		len(profiles[1].Sessions) != 1 || profiles[1].Sessions[0].ID != createdCodex.ID ||
		profiles[0].LastUsed < createdClaude.CreatedAt || profiles[1].LastUsed < createdCodex.CreatedAt {
		t.Fatalf("profiles = %#v", profiles)
	}
}

func TestProfileCreateTeachingErrorsDoNotLaunch(t *testing.T) {
	root := t.TempDir()
	manager, launcher, _ := newWorktreeTestManager(t, root)
	for _, profile := range []string{"Work", "work_home", strings.Repeat("a", 33), "two words"} {
		before := len(launcher.Launches)
		_, err := manager.Create(context.Background(), state.CreateSessionRequest{Cmd: "claude", Cwd: root, Profile: profile})
		if err == nil || !strings.Contains(err.Error(), "1-32 lowercase letters, digits, or hyphens") || len(launcher.Launches) != before {
			t.Fatalf("profile %q err=%v launches=%d->%d", profile, err, before, len(launcher.Launches))
		}
	}
	_, err := manager.Create(context.Background(), state.CreateSessionRequest{Cmd: "/bin/sh", Cwd: root, Profile: "work"})
	if err == nil || !strings.Contains(err.Error(), "only for Claude or Codex") {
		t.Fatalf("shell profile error = %v", err)
	}
}

func TestProfileConfigRootsReachLiveClaudeAndCodexWatchers(t *testing.T) {
	root := t.TempDir()
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(root, "ledger", "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	launcher := prototest.NewLauncher()
	manager := NewManager(testConfig(root), launcher, ManagerOptions{
		ActivityInterval: time.Hour, Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
		Notify: func(PushPayload) {},
	})
	t.Cleanup(manager.Close)

	const claudeID = "bbbbbbbb-1111-4222-8333-444444444444"
	claude, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "claude", Cwd: root, Profile: "watch-claude", Args: []string{"--session-id", claudeID},
	})
	if err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(claude.ConfigDir, "projects", watch.EncodeClaudeCWD(root), claudeID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(claudePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudePath, []byte(`{"type":"assistant","uuid":"profile-claude","message":{"role":"assistant","content":"watched"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	awaitCondition(t, func() bool {
		current, ok := manager.Get(claude.ID)
		return ok && current.ClaudeEventCount() == 1
	})

	codex, err := manager.Create(context.Background(), state.CreateSessionRequest{Cmd: "codex", Cwd: root, Profile: "watch-codex"})
	if err != nil {
		t.Fatal(err)
	}
	created := time.UnixMilli(codex.CreatedAt)
	codexPath := filepath.Join(codex.ConfigDir, "sessions", created.Format("2006"), created.Format("01"), created.Format("02"), "rollout-profile.jsonl")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o700); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"timestamp":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","type":"session_meta","payload":{"cwd":"` + root + `","timestamp":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}}`,
		`{"timestamp":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"profile codex watched"}]}}`,
	}
	if err := os.WriteFile(codexPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	awaitCondition(t, func() bool {
		current, ok := manager.Get(codex.ID)
		return ok && current.ClaudeEventCount() == 1
	})
}
