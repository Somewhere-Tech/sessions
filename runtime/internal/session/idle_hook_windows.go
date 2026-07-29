//go:build windows

package session

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func newIdleHookCommand(script string) *exec.Cmd {
	shell := os.Getenv("COMSPEC")
	if shell == "" {
		shell = "cmd.exe"
	}
	command := exec.Command(shell, "/d", "/s", "/c", script)
	command.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
		HideWindow:    true,
	}
	return command
}
