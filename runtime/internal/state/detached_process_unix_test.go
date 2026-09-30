//go:build !windows

package state

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The one attribute the whole promise rests on. Without Setsid the runner
// shares the daemon's process group, and anything that ends the daemon the way
// a shell or a service manager does — a signal to the group — ends every
// session on the machine with it.
func TestDetachedSysProcAttrStartsItsOwnSession(t *testing.T) {
	attributes := detachedSysProcAttr()
	if attributes == nil || !attributes.Setsid {
		t.Fatalf("detachedSysProcAttr() = %#v, want Setsid so the runner leads its own session", attributes)
	}
}

// A runner is ended through its process group, because it owns the provider
// process it launched; signalling the pid alone would leave the provider
// running with no runner attached to it.
func TestTerminateRunnerProcessEndsTheWholeGroup(t *testing.T) {
	script := filepath.Join(t.TempDir(), "runner.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 60 &\necho $! > \"$1\"\nwait\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	childPIDFile := filepath.Join(t.TempDir(), "child.pid")
	command := exec.Command(script, childPIDFile)
	command.SysProcAttr = detachedSysProcAttr()
	if err := command.Start(); err != nil {
		t.Fatalf("start probe: %v", err)
	}
	// The launcher collects the exit status in a goroutine so a finished runner
	// never lingers as a zombie; the probe is set up the same way, because a
	// zombie group member would make this test measure the harness instead of
	// the termination.
	waited := make(chan struct{})
	go func() { _ = command.Wait(); close(waited) }()
	t.Cleanup(func() {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-waited
	})
	if pgid, err := syscall.Getpgid(command.Process.Pid); err != nil || pgid != command.Process.Pid {
		t.Fatalf("probe pgid = %d (err %v), want its own group led by %d", pgid, err, command.Process.Pid)
	}
	child := waitForChildPID(t, childPIDFile)

	if err := terminateRunnerProcess(command.Process.Pid); err != nil {
		t.Fatalf("terminateRunnerProcess() = %v", err)
	}
	<-waited
	if syscall.Kill(child, 0) == nil {
		t.Fatal("the process the runner launched survived; only the runner itself was signalled")
	}
}

// Reap runs after a clean exit too, where there is nothing left to signal.
func TestTerminateRunnerProcessIsQuietWhenThereIsNothingToEnd(t *testing.T) {
	if err := terminateRunnerProcess(0); err != nil {
		t.Fatalf("terminateRunnerProcess(0) = %v", err)
	}
}

// assertOwnProcessGroup is what detachment means on Unix: the runner leads the
// group, so nothing aimed at the daemon's group reaches it.
func assertOwnProcessGroup(t *testing.T, pid int) {
	t.Helper()
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("Getpgid(%d) = %v", pid, err)
	}
	if pgid != pid {
		t.Fatalf("runner pgid = %d, want its own group %d: it is still in the daemon's group", pgid, pid)
	}
}

func waitForChildPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			if pid := parsePID(string(raw)); pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("probe never recorded the pid it launched at %s", path)
	return 0
}

func parsePID(raw string) int {
	pid := 0
	for _, character := range raw {
		if character < '0' || character > '9' {
			break
		}
		pid = pid*10 + int(character-'0')
	}
	return pid
}
