//go:build !windows

package session

import (
	"os/exec"
	"syscall"
)

func newIdleHookCommand(script string) *exec.Cmd {
	command := exec.Command("/bin/sh", "-c", script)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return command
}
