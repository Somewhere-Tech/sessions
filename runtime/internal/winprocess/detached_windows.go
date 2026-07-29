//go:build windows

package winprocess

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// DetachedCreationFlags keep a Sessions lifetime owner outside any viewer,
// installer, daemon, or test-harness Job Object while preserving a private
// process group and avoiding a console window.
const DetachedCreationFlags = windows.CREATE_BREAKAWAY_FROM_JOB |
	windows.CREATE_NEW_PROCESS_GROUP |
	windows.CREATE_NO_WINDOW

// ConfigureDetached preserves any caller-supplied Windows process attributes
// while adding the lifetime flags required by Sessions supervisors and
// runners.
func ConfigureDetached(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.CreationFlags |= DetachedCreationFlags
	command.SysProcAttr.HideWindow = true
}

// StartDetached fails closed when a parent Job Object forbids breakaway. A
// silently nested supervisor or runner could otherwise die when the viewer or
// installer closes.
func StartDetached(command *exec.Cmd) error {
	ConfigureDetached(command)
	if err := command.Start(); err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return fmt.Errorf(
				"Windows denied the independent Sessions process start; no process was started. A restrictive parent Job Object can forbid the required breakaway: close any installer-launched Sessions window and reopen Sessions from the Start menu, or sign out and back in, before retrying. If it still fails, verify this Windows user can run the staged Sessions runtime: %w",
				err,
			)
		}
		return err
	}
	return nil
}
