package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// newDelegatedAccountManager keeps every provider-home read inside the test:
// the Claude defaults look for MCP configuration under HOME.
func newDelegatedAccountManager(t *testing.T) (*Manager, *prototest.Launcher, *ledger.Store, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	for _, key := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"} {
		t.Setenv(key, "")
	}
	manager, launcher, store := newWorktreeTestManager(t, root)
	return manager, launcher, store, root
}

func lastLaunchEnv(t *testing.T, launcher *prototest.Launcher) map[string]string {
	t.Helper()
	if len(launcher.Launches) == 0 {
		t.Fatal("nothing was launched")
	}
	return launcher.Launches[len(launcher.Launches)-1].Env
}

func createAccountParent(t *testing.T, manager *Manager, root, cmd, profile string) state.SessionInfo {
	t.Helper()
	parent, err := manager.Create(context.Background(), state.CreateSessionRequest{Cmd: cmd, Cwd: root, Name: "manager", Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	return parent
}

func TestSameProviderChildStartsOnItsManagersAccount(t *testing.T) {
	for _, tool := range []struct{ cmd, key, other, override string }{
		{"claude", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "ANTHROPIC_API_KEY"},
		{"codex", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "OPENAI_API_KEY"},
	} {
		t.Run(tool.cmd, func(t *testing.T) {
			manager, launcher, store, root := newDelegatedAccountManager(t)
			parent := createAccountParent(t, manager, root, tool.cmd, "work")
			home := filepath.Join(manager.config.UserStateRoot, "profiles", tool.cmd, "work")

			// The CLI's shape: no account named, and a caller environment that
			// tries to point the provider somewhere else.
			child, err := manager.Create(context.Background(), state.CreateSessionRequest{
				Cmd: tool.cmd, Cwd: root, Name: "worker", CreatorSessionID: parent.ID,
				Env: map[string]string{"CLAUDE_CONFIG_DIR": "/caller", "CODEX_HOME": "/caller", "ANTHROPIC_API_KEY": "caller", "OPENAI_API_KEY": "caller"},
			})
			if err != nil {
				t.Fatal(err)
			}
			env := lastLaunchEnv(t, launcher)
			if env[tool.key] != home || child.Profile != "work" || child.ConfigDir != home {
				t.Fatalf("child account = %q/%q, %s=%q; want work at %s", child.Profile, child.ConfigDir, tool.key, env[tool.key], home)
			}
			// The provider's own ambient authentication must not replace the
			// inherited login, and the other provider's home is not set.
			for _, key := range []string{tool.other, tool.override} {
				if _, present := env[key]; present {
					t.Fatalf("child launch kept caller %s: %#v", key, env)
				}
			}
			metadata, err := state.ReadRunnerMetadata(filepath.Join(manager.config.RunnerStateDir, child.ID+".json"))
			if err != nil || metadata.Profile != "work" || metadata.ConfigDir != home {
				t.Fatalf("runner metadata = %#v, %v", metadata, err)
			}
			events, err := store.Events(context.Background(), child.ID)
			if err != nil {
				t.Fatal(err)
			}
			folded := ledger.Fold(events)
			if len(folded) != 1 || folded[0].Profile != "work" || folded[0].ConfigDir != home {
				t.Fatalf("child ledger account = %#v", folded)
			}
		})
	}
}

func TestChildAccountSelectionIsExplicitWhenNamed(t *testing.T) {
	manager, launcher, _, root := newDelegatedAccountManager(t)
	parent := createAccountParent(t, manager, root, "claude", "work")
	profiles := filepath.Join(manager.config.UserStateRoot, "profiles", "claude")

	named, err := manager.Create(context.Background(), state.CreateSessionRequest{Cmd: "claude", Cwd: root, CreatorSessionID: parent.ID, Profile: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	if env := lastLaunchEnv(t, launcher); named.Profile != "personal" || env["CLAUDE_CONFIG_DIR"] != filepath.Join(profiles, "personal") {
		t.Fatalf("named child = %q, CLAUDE_CONFIG_DIR=%q", named.Profile, env["CLAUDE_CONFIG_DIR"])
	}

	chosenDefault, err := manager.Create(context.Background(), state.CreateSessionRequest{Cmd: "claude", Cwd: root, CreatorSessionID: parent.ID, DefaultProfile: true})
	if err != nil {
		t.Fatal(err)
	}
	if env := lastLaunchEnv(t, launcher); chosenDefault.Profile != "" || chosenDefault.ConfigDir != "" || env["CLAUDE_CONFIG_DIR"] != "" {
		t.Fatalf("default child = %q/%q, CLAUDE_CONFIG_DIR=%q", chosenDefault.Profile, chosenDefault.ConfigDir, env["CLAUDE_CONFIG_DIR"])
	}

	before := len(launcher.Launches)
	for _, request := range []state.CreateSessionRequest{
		{Cmd: "claude", Cwd: root, CreatorSessionID: parent.ID, Profile: "personal", DefaultProfile: true},
		{Cmd: "claude", Cwd: root, CreatorSessionID: parent.ID, Profile: "Not_Valid"},
	} {
		if _, err := manager.Create(context.Background(), request); err == nil {
			t.Fatalf("request %#v was accepted", request)
		}
	}
	if len(launcher.Launches) != before {
		t.Fatalf("a refused account choice launched: %d -> %d", before, len(launcher.Launches))
	}
}

func TestChildAccountIsNeverInventedOrCarriedAcrossProviders(t *testing.T) {
	manager, launcher, _, root := newDelegatedAccountManager(t)
	unprofiled := createAccountParent(t, manager, root, "claude", "")
	claudeOnWork := createAccountParent(t, manager, root, "claude", "work")
	codexOnWork := createAccountParent(t, manager, root, "codex", "work")

	for _, request := range []state.CreateSessionRequest{
		{Cmd: "claude", Cwd: root, CreatorSessionID: unprofiled.ID},
		{Cmd: "codex", Cwd: root, CreatorSessionID: claudeOnWork.ID},
		{Cmd: "claude", Cwd: root, CreatorSessionID: codexOnWork.ID},
		{Cmd: "claude", Cwd: root, CreatorSessionID: claudeOnWork.ID, Kind: state.KindLane},
		{Cmd: "/bin/sh", Cwd: root, CreatorSessionID: claudeOnWork.ID},
	} {
		child, err := manager.Create(context.Background(), request)
		if err != nil {
			t.Fatalf("create %s child of %s: %v", request.Cmd, request.CreatorSessionID, err)
		}
		env := lastLaunchEnv(t, launcher)
		if child.Profile != "" || child.ConfigDir != "" || env["CLAUDE_CONFIG_DIR"] != "" || env["CODEX_HOME"] != "" {
			t.Fatalf("%s child (kind %q) got account %q/%q env=%#v", request.Cmd, request.Kind, child.Profile, child.ConfigDir, env)
		}
	}

	before := len(launcher.Launches)
	_, err := manager.Create(context.Background(), state.CreateSessionRequest{Cmd: "claude", Cwd: root, CreatorSessionID: "4d1a7f3e-0000-4000-8000-000000000000"})
	if err == nil || !strings.Contains(err.Error(), "creator") || len(launcher.Launches) != before {
		t.Fatalf("unknown parent err=%v launches=%d->%d", err, before, len(launcher.Launches))
	}
}

func TestReplayedChildCreateTouchesNoAccountHome(t *testing.T) {
	manager, launcher, _, root := newDelegatedAccountManager(t)
	parent := createAccountParent(t, manager, root, "codex", "work")
	const operation = "6f1c2b9a-3d4e-4f50-8a61-7b8c9d0e1f2a"
	first, err := manager.Create(context.Background(), state.CreateSessionRequest{Cmd: "codex", Cwd: root, CreatorSessionID: parent.ID, OperationID: operation})
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(manager.config.UserStateRoot, "profiles", "codex", "work")
	old := time.Unix(1_000_000, 0)
	if err := os.Chtimes(home, old, old); err != nil {
		t.Fatal(err)
	}
	before := len(launcher.Launches)

	for _, request := range []state.CreateSessionRequest{
		{Cmd: "codex", Cwd: root, CreatorSessionID: parent.ID, OperationID: operation},
		{Cmd: "codex", Cwd: root, CreatorSessionID: parent.ID, OperationID: operation, Profile: "elsewhere"},
	} {
		replayed, err := manager.Create(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if replayed.ID != first.ID || replayed.Start == nil || !replayed.Start.Replayed {
			t.Fatalf("replay = %#v, want %s", replayed, first.ID)
		}
	}
	if len(launcher.Launches) != before {
		t.Fatalf("a replay launched: %d -> %d", before, len(launcher.Launches))
	}
	if info, err := os.Stat(home); err != nil || !info.ModTime().Equal(old) {
		t.Fatalf("replay touched the inherited home: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(manager.config.UserStateRoot, "profiles", "codex", "elsewhere")); !os.IsNotExist(err) {
		t.Fatalf("replay created another account home: %v", err)
	}
}
