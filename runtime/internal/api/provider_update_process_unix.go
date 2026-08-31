//go:build !windows

package api

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Provider CLIs commonly launch a package manager child. CommandContext's
// default cancellation kills only the CLI parent, which can leave npm running
// forever while its inherited output pipe keeps the HTTP request open. Give
// the updater its own process group and cancel the complete tree.
func configureProviderUpdateCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = 5 * time.Second
}
