//go:build windows

package state

import (
	"testing"

	"golang.org/x/sys/windows"
)

// The Windows counterpart of setsid, for the purpose that matters: a group
// signal aimed at the daemon must not reach the runner, an installer-launched
// Job Object must not take the runner with it, and the runner must not flash a
// console window at the person.
func TestDetachedSysProcAttrCreatesAnIndependentProcessGroup(t *testing.T) {
	attributes := detachedSysProcAttr()
	if attributes == nil {
		t.Fatal("detachedSysProcAttr() = nil")
	}
	for name, flag := range map[string]uint32{
		"CREATE_NEW_PROCESS_GROUP":  windows.CREATE_NEW_PROCESS_GROUP,
		"CREATE_BREAKAWAY_FROM_JOB": windows.CREATE_BREAKAWAY_FROM_JOB,
		"CREATE_NO_WINDOW":          windows.CREATE_NO_WINDOW,
	} {
		if attributes.CreationFlags&flag == 0 {
			t.Errorf("creation flags %#x do not include %s", attributes.CreationFlags, name)
		}
	}
	if !attributes.HideWindow {
		t.Error("HideWindow = false; a runner must not show a console window")
	}
}

// Windows has no process groups to read back the way Unix does; the creation
// flags asserted above are what detachment means here, and they are checked
// where they are set.
func assertOwnProcessGroup(t *testing.T, pid int) {
	t.Helper()
	if pid <= 0 {
		t.Fatalf("runner pid = %d", pid)
	}
}

func TestTerminateRunnerProcessIsQuietWhenThereIsNothingToEnd(t *testing.T) {
	if err := terminateRunnerProcess(0); err != nil {
		t.Fatalf("terminateRunnerProcess(0) = %v", err)
	}
}
