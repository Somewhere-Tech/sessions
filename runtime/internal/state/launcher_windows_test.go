//go:build windows

package state

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func startProbeProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	command := exec.Command("ping.exe", "-n", "60", "127.0.0.1")
	if err := command.Start(); err != nil {
		t.Skipf("cannot start a probe process on this host: %v", err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	})
	return command
}

func processIsAlive(t *testing.T, pid int) bool {
	t.Helper()
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	event, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && event == uint32(windows.WAIT_TIMEOUT)
}

// Registry calls Reap after a failed launch so a runner that never published
// its endpoint cannot keep spinning invisibly. The Windows implementation was a
// no-op, which left exactly that orphan behind.
func TestWindowsLauncherReapEndsTheRunnerItStarted(t *testing.T) {
	probe := startProbeProcess(t)
	launcher := NewWindowsLauncher(Config{RunnerStateDir: t.TempDir()})
	launcher.remember("session-a", probe.Process.Pid)

	if err := launcher.Reap("session-a"); err != nil {
		t.Fatalf("Reap() = %v", err)
	}
	if processIsAlive(t, probe.Process.Pid) {
		t.Fatal("Reap() left the started runner running")
	}
}

// Windows reuses process ids. Reap must terminate only a process it can
// positively identify as that runner; a wrong guess ends an unrelated process
// belonging to the signed-in user.
func TestWindowsLauncherReapRefusesAPidItCannotIdentify(t *testing.T) {
	probe := startProbeProcess(t)
	launcher := NewWindowsLauncher(Config{RunnerStateDir: t.TempDir()})
	launcher.mu.Lock()
	launcher.started["session-b"] = launchedRunner{
		pid:     uint32(probe.Process.Pid),
		created: windows.Filetime{LowDateTime: 1, HighDateTime: 1},
	}
	launcher.mu.Unlock()

	if err := launcher.Reap("session-b"); err != nil {
		t.Fatalf("Reap() = %v, want a quiet no-op for an unidentifiable pid", err)
	}
	if !processIsAlive(t, probe.Process.Pid) {
		t.Fatal("Reap() terminated a process whose identity did not match the runner")
	}
}

func TestWindowsLauncherReapOfAnUnknownSessionIsNotAnError(t *testing.T) {
	launcher := NewWindowsLauncher(Config{RunnerStateDir: t.TempDir()})
	// Reap also runs after a clean exit, where its nil result is what lets the
	// ledger record the lane as reaped.
	if err := launcher.Reap("never-launched"); err != nil {
		t.Fatalf("Reap(unknown) = %v", err)
	}
}

// launchd points StandardOutPath/StandardErrorPath at <id>.log. Without the
// same file on Windows, a runner that dies before publishing its pipe leaves no
// diagnostic at all.
func TestOpenRunnerLogCreatesAnAppendableSessionLog(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "runners")
	id := "2f577cd7-565b-4861-8ea2-c77c39a20e24"

	first, err := openRunnerLog(directory, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.WriteString("first\n"); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := openRunnerLog(directory, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.WriteString("second\n"); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	path := For(directory, id).Log
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(contents); !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Fatalf("runner log = %q, want both writes appended", got)
	}
}
