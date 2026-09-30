//go:build windows

package state

import (
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/somewhere-tech/sessions/runtime/internal/winprocess"
)

// detachedSysProcAttr gives the runner its own process group, keeps it out of
// any Job Object the daemon or an installer was started in, and gives it no
// console window.
//
// CREATE_NEW_PROCESS_GROUP is the Windows counterpart of setsid for the
// purpose that matters here: a Ctrl-Break or group signal aimed at the daemon
// does not reach the runner. CREATE_NO_WINDOW rather than DETACHED_PROCESS,
// because the runner still needs a console of its own to host a provider PTY,
// and CREATE_BREAKAWAY_FROM_JOB because a Sessions installed from an
// installer-launched window would otherwise die with that window.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: winprocess.DetachedCreationFlags,
		HideWindow:    true,
	}
}

// terminateRunnerProcess ends the runner this daemon started.
//
// Windows has no signal that reaches a process group without a shared console,
// so this terminates the process itself. The runner owns its provider through a
// Job Object, which Windows tears down with it; that is what makes ending the
// runner enough.
func terminateRunnerProcess(pid int) error {
	if pid <= 0 {
		return nil
	}
	handle, err := windows.OpenProcess(
		windows.PROCESS_TERMINATE|windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false, uint32(pid),
	)
	if err != nil {
		// Gone, or not ours to touch. Either way there is nothing to end.
		return nil
	}
	defer windows.CloseHandle(handle)
	if err := windows.TerminateProcess(handle, 1); err != nil {
		if event, waitErr := windows.WaitForSingleObject(handle, 0); waitErr == nil && event == windows.WAIT_OBJECT_0 {
			return nil
		}
		return err
	}
	_, _ = windows.WaitForSingleObject(handle, 5_000)
	return nil
}
