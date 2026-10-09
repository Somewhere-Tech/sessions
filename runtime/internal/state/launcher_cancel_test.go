package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
)

func TestLaunchdBootstrapHonorsCancellationWithoutWaitingForSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	root := t.TempDir()
	// Only this fixture process is terminated; it never talks to launchd.
	if err := os.WriteFile(filepath.Join(root, "launchctl"), []byte("#!/bin/sh\nexec /bin/sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	launcher := NewLaunchdLauncher(Config{RunnerStateDir: root, LaunchAgentsDir: root})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := launcher.Launch(ctx, proto.LaunchRequest{
		Info: proto.RunnerInfo{ID: "isolated-cancel-test", Cmd: "/bin/sh", Cwd: root},
		Env:  map[string]string{"PATH": "/bin:/usr/bin"},
	})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 2*time.Second {
		t.Fatalf("bootstrap ignored deadline: elapsed=%s err=%v", time.Since(started), err)
	}
}

func TestUncertainLaunchdCleanupPreservesTheRegistration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "launchctl"), []byte("#!/bin/sh\nexec /bin/sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	const id = "isolated-reap-test"
	path := plistPath(root, id)
	if err := os.WriteFile(path, []byte("isolated fixture, not loaded in launchd"), 0o600); err != nil {
		t.Fatal(err)
	}
	launcher := NewLaunchdLauncher(Config{RunnerStateDir: root, LaunchAgentsDir: root})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := launcher.reapContext(ctx, id); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cleanup did not report uncertain outcome: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("uncertain cleanup removed recovery registration: %v", err)
	}
}
