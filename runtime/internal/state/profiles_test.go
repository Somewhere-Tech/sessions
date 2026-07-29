package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
)

func TestProfileMetadataSurvivesRunnerDiscovery(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "profiles", "claude", "work")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := Config{
		DefaultShell: "/bin/sh", DefaultCwd: root, DefaultCols: 80, DefaultRows: 24,
		RunnerStateDir: filepath.Join(root, "runners"), LaunchAgentsDir: filepath.Join(root, "agents"),
	}
	launcher := prototest.NewLauncher()
	first := NewRegistry(config, launcher)
	created, err := first.Create(context.Background(), CreateSessionRequest{
		Cmd: "claude", Cwd: root, Profile: "work", ConfigDir: configDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.RunnerStateDir, created.ID+".sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	second := NewRegistry(config, launcher)
	if err := second.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	sessions := second.List(false)
	if len(sessions) != 1 || sessions[0].Profile != "work" || sessions[0].ConfigDir != configDir {
		t.Fatalf("discovered profile sessions = %#v", sessions)
	}
}
