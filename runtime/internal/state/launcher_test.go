package state

import (
	"os"
	"path/filepath"
	"reflect"
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
