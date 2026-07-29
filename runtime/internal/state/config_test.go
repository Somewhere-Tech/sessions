package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigFromEnvParityAndScratchIsolation(t *testing.T) {
	root := t.TempDir()
	runners := filepath.Join(root, "state", "runners")
	web := filepath.Join(root, "web")
	runner := filepath.Join(root, "runner")
	if err := os.WriteFile(runner, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("SESSIONS_HOST", "100.64.1.2")
	t.Setenv("SESSIONS_PORT", "9911")
	t.Setenv("SESSIONS_STATE_DIR", runners)
	t.Setenv("SESSIONS_WEB_DIR", web)
	t.Setenv("SESSIONS_RUNNER", runner)
	t.Setenv("SHELL", "/bin/zsh")
	config, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if config.Host != "100.64.1.2" || config.Port != 9911 || config.DefaultShell != "/bin/zsh" {
		t.Fatalf("unexpected env config: %#v", config)
	}
	if config.RunnerStateDir != runners || config.StateRoot != filepath.Dir(runners) {
		t.Fatalf("unexpected state paths: root=%s runners=%s", config.StateRoot, config.RunnerStateDir)
	}
	if config.RunnerPath != runner {
		t.Fatalf("runner path = %q, want %q", config.RunnerPath, runner)
	}
	tokenPath, err := LocalTokenPathFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if tokenPath != config.TokenPath {
		t.Fatalf("CLI token path = %q, daemon token path = %q", tokenPath, config.TokenPath)
	}
	for _, path := range []string{config.TokenPath, config.OpenPath, config.SettingsPath, config.LaunchAgentsDir, config.WebDir} {
		if path == "" || !filepath.IsAbs(path) {
			t.Errorf("path is not absolute: %q", path)
		}
	}
	if _, err := os.Stat(runners); !os.IsNotExist(err) {
		t.Fatalf("loading config mutated the state dir: %v", err)
	}
}

func TestResolveRunnerPathCandidates(t *testing.T) {
	root := t.TempDir()
	daemon := filepath.Join(root, "sessionsd-darwin-arm64")
	unsuffixed := filepath.Join(root, "sessions-runner")
	suffixed := filepath.Join(root, "sessions-runner-darwin-arm64")
	explicit := filepath.Join(root, "custom-runner")
	for _, path := range []string{unsuffixed, suffixed, explicit} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name       string
		explicit   string
		executable string
		prepare    func(t *testing.T)
		want       string
	}{
		{name: "explicit executable", explicit: explicit, executable: daemon, want: explicit},
		{name: "co-located unsuffixed wins", executable: daemon, want: unsuffixed},
		{
			name: "platform-suffixed fallback", executable: daemon, want: suffixed,
			prepare: func(t *testing.T) {
				t.Helper()
				if err := os.Chmod(unsuffixed, 0o600); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(unsuffixed, 0o700) })
			},
		},
		{name: "missing explicit does not fall back", explicit: filepath.Join(root, "missing"), executable: daemon},
		{name: "no co-located executable", executable: filepath.Join(t.TempDir(), "sessionsd")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.prepare != nil {
				test.prepare(t)
			}
			if got := resolveRunnerPathFrom(test.explicit, test.executable, "darwin", "arm64"); got != test.want {
				t.Fatalf("resolveRunnerPathFrom() = %q, want %q", got, test.want)
			}
		})
	}
}
