//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestUnixExplicitEndStopsRunnerOwnedProcessTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	command := exec.Command("/bin/sh", "-c", "sleep 30 & echo $! > \"$1\"; wait", "sessions-test", pidFile)
	process, err := startPlatformChildProcess(command, 80, 24, true)
	if err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = process.ForceKill()
			_ = process.Wait(true)
		}
	})

	descendantPID := waitForUnixDescendantPID(t, pidFile)
	if err := syscall.Kill(descendantPID, 0); err != nil {
		t.Fatalf("descendant %d was not alive before End: %v", descendantPID, err)
	}
	if err := process.RequestStop(); err != nil {
		t.Fatalf("End runner-owned tree: %v", err)
	}
	_ = process.Wait(true)
	waited = true

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(descendantPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("runner-owned descendant %d survived explicit End", descendantPID)
}

func waitForUnixDescendantPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr != nil {
				t.Fatalf("parse descendant pid: %v", parseErr)
			}
			return pid
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read descendant pid: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for descendant pid")
	return 0
}
