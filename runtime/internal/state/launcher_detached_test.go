package state

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
)

func detachedConfig(t *testing.T, runnerPath string) Config {
	t.Helper()
	root := t.TempDir()
	return Config{
		RunnerStateDir:  filepath.Join(root, "runners"),
		LaunchAgentsDir: filepath.Join(root, "agents"),
		StateRoot:       root,
		UserStateRoot:   root,
		RunnerPath:      runnerPath,
	}
}

// A program that runs long enough to be found, ended, and asked about.
func probeRunner(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the probe runner is a POSIX shell script")
	}
	path := filepath.Join(t.TempDir(), "runner.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// Prepare is the durable half of a launch: the same boot-scoped permit launchd
// gets from the plist, plus the launch record that stands in for the plist on a
// machine that has none.
func TestDetachedPrepareRecordsThePermitAndTheLaunch(t *testing.T) {
	runnerPath := probeRunner(t, "sleep 30\n")
	config := detachedConfig(t, runnerPath)
	launcher := NewDetachedLauncher(config)
	request := proto.LaunchRequest{
		Info: proto.RunnerInfo{ID: "prepare-me", Cwd: t.TempDir()},
		Env:  map[string]string{"RUNNER_ID": "prepare-me"},
	}

	if err := launcher.Prepare(request); err != nil {
		t.Fatalf("Prepare() = %v", err)
	}
	paths := For(config.RunnerStateDir, "prepare-me")
	if _, err := os.Stat(paths.KeepAlive); err != nil {
		t.Fatalf("no restart permit: %v", err)
	}
	spec, err := readLaunchSpec(paths.Launch)
	if err != nil {
		t.Fatalf("read launch record: %v", err)
	}
	if len(spec.Program) == 0 || spec.Program[0] != runnerPath {
		t.Fatalf("launch record program = %#v, want the runner binary", spec.Program)
	}
	// The runner learns the reboot policy from its environment, exactly as it
	// does from the plist launchd writes.
	if spec.Env["RUNNER_RESTART_POLICY"] != "boot-scoped" {
		t.Fatalf("launch record env = %#v, want the boot-scoped restart policy", spec.Env)
	}
}

// The failure this launcher exists to end was a bare `exec: "launchctl":
// executable file not found in $PATH`. Every refusal here names the launcher
// and the platform instead.
func TestDetachedPreflightNamesTheLauncherAndThePlatform(t *testing.T) {
	config := detachedConfig(t, filepath.Join(t.TempDir(), "no-such-runner"))
	launcher := NewDetachedLauncher(config)

	err := launcher.Preflight(proto.LaunchRequest{Info: proto.RunnerInfo{ID: "x", Cmd: "sh"}})
	if err == nil {
		t.Fatal("Preflight() accepted a missing runner binary")
	}
	if !strings.Contains(err.Error(), "detached") || !strings.Contains(err.Error(), runtime.GOOS) {
		t.Fatalf("Preflight() = %v, want the launcher and the platform named", err)
	}

	config = detachedConfig(t, probeRunner(t, "sleep 1\n"))
	launcher = NewDetachedLauncher(config)
	err = launcher.Preflight(proto.LaunchRequest{
		Info: proto.RunnerInfo{ID: "x", Cmd: "definitely-not-installed", Cwd: t.TempDir()},
		Env:  map[string]string{"PATH": "/nonexistent"},
	})
	if err == nil || !strings.Contains(err.Error(), "definitely-not-installed") {
		t.Fatalf("Preflight() = %v, want the unusable session command named", err)
	}
	if !strings.Contains(err.Error(), runtime.GOOS) {
		t.Fatalf("Preflight() = %v, want the platform named", err)
	}
}

// Starting is detaching: the runner leads its own session, so a signal aimed
// at the daemon's process group cannot reach it. Reap ends it again and takes
// the launch record with it.
func TestDetachedStartDetachesAndReapEnds(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	runnerPath := probeRunner(t, "echo running > \""+marker+"\"\nsleep 30\n")
	config := detachedConfig(t, runnerPath)
	launcher := NewDetachedLauncher(config)
	request := proto.LaunchRequest{
		Info: proto.RunnerInfo{ID: "detach-me", Cwd: t.TempDir()},
		Env:  map[string]string{"RUNNER_ID": "detach-me"},
	}
	if err := launcher.Prepare(request); err != nil {
		t.Fatal(err)
	}
	spec, err := readLaunchSpec(For(config.RunnerStateDir, "detach-me").Launch)
	if err != nil {
		t.Fatal(err)
	}
	if err := launcher.start("detach-me", spec); err != nil {
		t.Fatalf("start() = %v", err)
	}
	waitForFile(t, marker)
	// The pid is durable, so Reap can end what was started and a later daemon
	// can say which process belonged to which session.
	recorded, err := readLaunchSpec(For(config.RunnerStateDir, "detach-me").Launch)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.PID <= 0 {
		t.Fatal("the launch record names no pid, so Reap could never end what it started")
	}
	assertOwnProcessGroup(t, recorded.PID)

	if err := launcher.Reap("detach-me"); err != nil {
		t.Fatalf("Reap() = %v", err)
	}
	paths := For(config.RunnerStateDir, "detach-me")
	for _, path := range []string{paths.KeepAlive, paths.Launch} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s survived Reap: %v", filepath.Base(path), err)
		}
	}
	// The runner's log is deliberately left behind for diagnosis.
	if _, err := os.Stat(paths.Log); err != nil {
		t.Fatalf("Reap removed the runner log: %v", err)
	}
}

// Waking has to know what to start again. Without launchd there is no plist to
// kickstart, so a session with no launch record is refused by name rather than
// started from a guess.
func TestDetachedWakeRefusesWithoutALaunchRecord(t *testing.T) {
	config := detachedConfig(t, probeRunner(t, "sleep 1\n"))
	launcher := NewDetachedLauncher(config)
	paths := For(config.RunnerStateDir, "paused")
	if err := EnsureDir(config.RunnerStateDir); err != nil {
		t.Fatal(err)
	}
	if err := WriteRestorePending(paths.RestorePending, "paused", "paused after restart"); err != nil {
		t.Fatal(err)
	}

	_, err := launcher.Wake(t.Context(), "paused")
	if err == nil {
		t.Fatal("Wake() started something with no record of what it was")
	}
	if !strings.Contains(err.Error(), "sessions resume paused") {
		t.Fatalf("Wake() = %v, want the refusal to say what the person can do instead", err)
	}
	// The marker stays: a session that could not be woken is still paused, not
	// unknown.
	if _, statErr := os.Stat(paths.RestorePending); statErr != nil {
		t.Fatalf("the paused marker was lost: %v", statErr)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never appeared; the runner did not start", path)
}
