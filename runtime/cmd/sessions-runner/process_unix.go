//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"github.com/creack/pty"
)

type unixChildProcess struct {
	command  *exec.Cmd
	terminal *os.File
	output   *os.File
	headless bool
}

func startPlatformChildProcess(command *exec.Cmd, cols, rows int, headless bool) (childProcess, error) {
	child := &unixChildProcess{command: command, headless: headless}
	if headless {
		// A headless runner owns one complete command tree. Put the command in
		// its own process group so explicit End can terminate that tree just as
		// the Windows runner terminates its Job Object. Without this, the shell
		// exits but grandchildren such as dev servers survive unpredictably.
		if command.SysProcAttr == nil {
			command.SysProcAttr = &syscall.SysProcAttr{}
		}
		command.SysProcAttr.Setpgid = true
		output, writePipe, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		child.output = output
		command.Stdin = nil
		command.Stdout = writePipe
		command.Stderr = writePipe
		err = command.Start()
		_ = writePipe.Close()
		if err != nil {
			_ = output.Close()
			return nil, err
		}
		return child, nil
	}
	terminal, err := pty.StartWithSize(command, &pty.Winsize{
		Rows: uint16(rows),
		Cols: uint16(cols),
	})
	if err != nil {
		return nil, err
	}
	child.terminal = terminal
	child.output = terminal
	return child, nil
}

func (p *unixChildProcess) PID() int {
	return p.command.Process.Pid
}

func (p *unixChildProcess) Read(buffer []byte) (int, error) {
	return p.output.Read(buffer)
}

func (p *unixChildProcess) Write(buffer []byte) (int, error) {
	if p.terminal == nil {
		return 0, errors.New("headless session does not accept terminal input")
	}
	return p.terminal.Write(buffer)
}

func (p *unixChildProcess) Resize(cols, rows int) error {
	if p.terminal == nil {
		return nil
	}
	return pty.Setsize(p.terminal, &pty.Winsize{
		Rows: uint16(rows),
		Cols: uint16(cols),
	})
}

func (p *unixChildProcess) RequestStop() error {
	if p.command.Process == nil {
		return os.ErrProcessDone
	}
	return p.signalOwnedTree(syscall.SIGHUP)
}

func (p *unixChildProcess) ForceKill() error {
	if p.command.Process == nil {
		return os.ErrProcessDone
	}
	return p.signalOwnedTree(syscall.SIGKILL)
}

func (p *unixChildProcess) signalOwnedTree(signal syscall.Signal) error {
	// PTY children are session leaders (creack/pty sets Setsid); headless
	// children are process-group leaders above. A negative pid therefore
	// targets the complete runner-owned tree. A process that deliberately
	// starts a new session has explicitly detached and is outside this tree.
	err := syscall.Kill(-p.command.Process.Pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

func (p *unixChildProcess) CloseOutput() error {
	if p.output == nil {
		return nil
	}
	return p.output.Close()
}

func (p *unixChildProcess) Wait(headless bool) exitInfo {
	waitErr := p.command.Wait()
	processState := p.command.ProcessState
	if processState != nil {
		if status, ok := processState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			signalText := strconv.Itoa(int(status.Signal()))
			// node-pty reports exitCode=0 alongside the numeric signal for a
			// signal-terminated PTY. Keep that slightly unusual pairing for
			// byte-level interop with runner.ts.
			code := 0
			if headless {
				code = 128 + int(status.Signal())
			}
			return exitInfo{Code: &code, Signal: &signalText}
		}
		code := processState.ExitCode()
		return exitInfo{Code: &code, Signal: nil}
	}
	if waitErr != nil {
		signalText := waitErr.Error()
		return exitInfo{Code: nil, Signal: &signalText}
	}
	code := 0
	return exitInfo{Code: &code, Signal: nil}
}
