package state

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
)

func TestNativeRunnerAssociationIsScopedToManagedJobs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	managed := filepath.Join(root, "Library", "Application Support", "Sessions", "runtime")
	agents := filepath.Join(root, "Library", "LaunchAgents")
	tests := []struct {
		name, runner, agents string
		managed              bool
	}{
		{"stable", filepath.Join(managed, "sessions-runner"), agents, true},
		{"versioned", filepath.Join(managed, "v1", "sessions-runner"), agents, true},
		{"scratch", filepath.Join(root, "scratch", "sessions-runner"), agents, false},
		{"scratch agents", filepath.Join(managed, "sessions-runner"), filepath.Join(root, "agents"), false},
		{"prefix sibling", filepath.Join(managed+"-other", "sessions-runner"), agents, false},
		{"different program", filepath.Join(managed, "other"), agents, false},
		{"traversal", filepath.Join(managed, "..", "sessions-runner"), agents, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := nativeRunnerAppAssociation(Config{RunnerPath: test.runner, LaunchAgentsDir: test.agents})
			want := ""
			if test.managed && runtime.GOOS == "darwin" {
				want = "tech.somewhere.sessions"
			}
			if got != want {
				t.Fatalf("association = %q, want %q", got, want)
			}
		})
	}
}

func TestNativeRunnerPrepareAssociatesAppWithoutChangingProviderPolicy(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS native app association")
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	runner := filepath.Join(root, "Library", "Application Support", "Sessions", "runtime", "sessions-runner")
	if err := os.MkdirAll(filepath.Dir(runner), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runner, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := Config{RunnerPath: runner, LaunchAgentsDir: filepath.Join(root, "Library", "LaunchAgents"), RunnerStateDir: filepath.Join(root, "runners")}
	if err := os.MkdirAll(config.RunnerStateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"RUNNER_ARGS_JSON": `["--permission-mode","default"]`}
	request := proto.LaunchRequest{Info: proto.RunnerInfo{ID: "native-association", Cwd: root}, Env: env}
	if err := NewLaunchdLauncher(config).Prepare(request); err != nil {
		t.Fatal(err)
	}
	value, err := os.ReadFile(RunnerPlistPath(config.LaunchAgentsDir, request.Info.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(value), "<key>AssociatedBundleIdentifiers</key>\n  <array>\n    <string>tech.somewhere.sessions</string>") {
		t.Fatalf("missing native association: %s", value)
	}
	if env["RUNNER_ARGS_JSON"] != `["--permission-mode","default"]` {
		t.Fatalf("provider policy changed: %v", env)
	}
	if mode, err := os.Stat(RunnerPlistPath(config.LaunchAgentsDir, request.Info.ID)); err != nil || mode.Mode().Perm() != 0o600 {
		t.Fatalf("plist is not private: %v, %v", mode, err)
	}
}

func TestRunnerAssociationXMLIsOptionalAndEscaped(t *testing.T) {
	if value := plistXML(plistArgs{ID: "scratch"}); strings.Contains(value, "AssociatedBundleIdentifiers") {
		t.Fatal("scratch plist borrowed native app identity")
	}
	value := plistXML(plistArgs{ID: "test", AppBundleID: "app<&>"})
	if !strings.Contains(value, "<string>app&lt;&amp;&gt;</string>") {
		t.Fatalf("unescaped association: %s", value)
	}
}
