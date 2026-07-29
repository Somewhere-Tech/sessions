package main

import (
	"io"
	"os/exec"
)

// childProcess is the deliberately small boundary between the portable runner
// protocol and each operating system's process/terminal implementation.
type childProcess interface {
	io.Reader
	io.Writer
	PID() int
	Resize(cols, rows int) error
	RequestStop() error
	ForceKill() error
	CloseOutput() error
	Wait(headless bool) exitInfo
}

func startChildProcess(command *exec.Cmd, cols, rows int, headless bool) (childProcess, error) {
	return startPlatformChildProcess(command, cols, rows, headless)
}
