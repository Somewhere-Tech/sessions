package state

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
)

func TestLaunchdRunnerProgramArguments(t *testing.T) {
	root := t.TempDir()
	runner := filepath.Join(root, "runner")
	if err := os.WriteFile(runner, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		path string
		want []string
	}{
		{name: "bare relative is refused", path: "runner"},
		{name: "missing absolute is refused", path: filepath.Join(root, "missing")},
		{name: "absolute executable", path: runner, want: []string{runner}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			launcher := NewLaunchdLauncher(Config{RunnerPath: test.path})
			if got := launcher.ProgramArguments(proto.LaunchRequest{}); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("ProgramArguments() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestLaunchdPrepareWritesBootScopedRestartContract(t *testing.T) {
	root := t.TempDir()
	runner := filepath.Join(root, "sessions-runner")
	if err := os.WriteFile(runner, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := Config{
		RunnerPath: runner, RunnerStateDir: filepath.Join(root, "runners"),
		LaunchAgentsDir: filepath.Join(root, "agents"),
	}
	id := "11111111-2222-4333-8444-555555555555"
	env := map[string]string{"RUNNER_ID": id}
	launcher := NewLaunchdLauncher(config)
	if err := launcher.Prepare(proto.LaunchRequest{
		Info: proto.RunnerInfo{ID: id, Cwd: root}, Env: env,
	}); err != nil {
		t.Fatal(err)
	}
	if env["RUNNER_RESTART_POLICY"] != "boot-scoped" {
		t.Fatalf("launch environment did not receive restart policy: %#v", env)
	}
	paths := For(config.RunnerStateDir, id)
	permit, err := readRestartPermit(paths.KeepAlive)
	if err != nil || permit.BootID == "" {
		t.Fatalf("restart permit = %#v, %v", permit, err)
	}
	plist, err := os.ReadFile(RunnerPlistPath(config.LaunchAgentsDir, id))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<key>RunAtLoad</key>\n  <false/>", "<key>PathState</key>",
		paths.KeepAlive, "<key>RUNNER_RESTART_POLICY</key>", "<string>boot-scoped</string>",
	} {
		if !strings.Contains(string(plist), want) {
			t.Fatalf("runner plist missing %q:\n%s", want, plist)
		}
	}
}

func TestRunnerCommandPathUsesRunnerEnvironment(t *testing.T) {
	root := t.TempDir()
	userBin := filepath.Join(root, ".local", "bin")
	if err := os.MkdirAll(userBin, 0o700); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(userBin, "claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, ok := runnerCommandPath("claude", root, userBin+":/usr/bin"); !ok || got != claude {
		t.Fatalf("runnerCommandPath(claude) = %q, %v; want %q, true", got, ok, claude)
	}
	if _, ok := runnerCommandPath("missing-agent", root, userBin+":/usr/bin"); ok {
		t.Fatal("runnerCommandPath unexpectedly found missing agent")
	}
}
