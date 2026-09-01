//go:build windows

package api

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// Windows package-manager descendants are not terminated by Process.Kill.
// taskkill /T follows the process tree; resolving it from System32 avoids a
// PATH-controlled executable at this privileged update boundary.
func configureProviderUpdateCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
		HideWindow:    true,
	}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		systemDirectory, err := windows.GetSystemDirectory()
		if err == nil {
			killer := exec.Command(
				filepath.Join(systemDirectory, "taskkill.exe"),
				"/PID", strconv.Itoa(command.Process.Pid), "/T", "/F",
			)
			killer.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
			if killErr := killer.Run(); killErr == nil {
				return nil
			}
		}
		err = command.Process.Kill()
		if errors.Is(err, os.ErrProcessDone) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = 5 * time.Second
}
