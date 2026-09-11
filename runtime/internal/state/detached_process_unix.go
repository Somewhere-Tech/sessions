//go:build !windows

package state

import (
	"errors"
	"syscall"
	"time"
)

// detachedSysProcAttr puts the runner in its own session, which makes it the
// leader of its own process group and detaches it from the daemon's
// controlling terminal.
//
// This is the whole reason a Linux session survives: without Setsid the runner
// shares the daemon's process group, and anything that ends the daemon the way
// a shell, a service manager, or a test harness does — a signal to the group —
// ends every session on the machine with it.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// terminateRunnerProcess ends a runner and the provider process it owns.
//
// The signal goes to the process group, because the runner is the leader of the
// group setsid gave it and the provider it launched is inside that group;
// signalling the pid alone would leave the provider running with no runner.
// SIGTERM first, so a runner can write its completion manifest, then SIGKILL
// for one that does not go.
func terminateRunnerProcess(pid int) error {
	if pid <= 0 {
		return nil
	}
	group := pid
	if pgid, err := syscall.Getpgid(pid); err == nil {
		// Only a group this process leads is ours to end. A pid that is not its
		// own group leader is not a runner we started with setsid, and killing
		// its group would reach unrelated processes.
		if pgid != pid {
			return nil
		}
		group = pgid
	}
	if err := syscall.Kill(-group, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-group, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(-group, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
