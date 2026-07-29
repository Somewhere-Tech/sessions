//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/somewhere-tech/sessions/runtime/internal/winconpty"
)

const windowsStopGrace = 1500 * time.Millisecond
const windowsProcThreadAttributeJobList = 0x0002000d

type windowsConPTYProcess struct {
	pid           int
	process       windows.Handle
	job           windows.Handle
	pseudoconsole windows.Handle
	input         *os.File
	output        *os.File

	stopOnce sync.Once
	stopErr  error
	waitOnce sync.Once
	waitInfo exitInfo
}

type windowsHeadlessProcess struct {
	command *exec.Cmd
	job     windows.Handle
	output  *os.File

	stopOnce sync.Once
	stopErr  error
}

func startPlatformChildProcess(command *exec.Cmd, cols, rows int, headless bool) (childProcess, error) {
	if headless {
		return startWindowsHeadlessProcess(command)
	}
	return startWindowsConPTYProcess(command, cols, rows)
}

func startWindowsConPTYProcess(command *exec.Cmd, cols, rows int) (_ childProcess, resultErr error) {
	if cols < 1 || rows < 1 || cols > 32767 || rows > 32767 {
		return nil, fmt.Errorf("invalid ConPTY size %dx%d", cols, rows)
	}
	var ptyInput, inputWrite, outputRead, ptyOutput windows.Handle
	if err := windows.CreatePipe(&ptyInput, &inputWrite, nil, 0); err != nil {
		return nil, fmt.Errorf("create ConPTY input pipe: %w", err)
	}
	defer func() {
		if resultErr != nil {
			closeWindowsHandles(ptyInput, inputWrite, outputRead, ptyOutput)
		}
	}()
	if err := windows.CreatePipe(&outputRead, &ptyOutput, nil, 0); err != nil {
		return nil, fmt.Errorf("create ConPTY output pipe: %w", err)
	}

	var pseudoconsole windows.Handle
	size := windows.Coord{X: int16(cols), Y: int16(rows)}
	if err := windows.CreatePseudoConsole(size, ptyInput, ptyOutput, 0, &pseudoconsole); err != nil {
		return nil, fmt.Errorf("create ConPTY: %w", err)
	}
	defer func() {
		if resultErr != nil {
			windows.ClosePseudoConsole(pseudoconsole)
		}
	}()

	job, err := newWindowsRunnerJob()
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			_ = windows.CloseHandle(job)
		}
	}()

	attributes, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		return nil, fmt.Errorf("allocate ConPTY process attributes: %w", err)
	}
	defer attributes.Delete()
	if err := winconpty.SetPseudoConsoleAttribute(attributes, pseudoconsole); err != nil {
		return nil, fmt.Errorf("attach ConPTY process attribute: %w", err)
	}
	if err := attributes.Update(
		windowsProcThreadAttributeJobList,
		unsafe.Pointer(&job),
		unsafe.Sizeof(job),
	); err != nil {
		return nil, fmt.Errorf("attach runner Job process attribute: %w", err)
	}

	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(command.Args))
	if err != nil {
		return nil, fmt.Errorf("encode child command line: %w", err)
	}
	var currentDirectory *uint16
	if command.Dir != "" {
		currentDirectory, err = windows.UTF16PtrFromString(command.Dir)
		if err != nil {
			return nil, fmt.Errorf("encode child working directory: %w", err)
		}
	}
	environment := command.Env
	if environment == nil {
		environment = os.Environ()
	}
	environmentBlock, err := windowsEnvironmentBlock(environment)
	if err != nil {
		return nil, err
	}

	startup := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb:    uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
			Flags: windows.STARTF_USESTDHANDLES,
		},
		ProcThreadAttributeList: attributes.List(),
	}
	var processInfo windows.ProcessInformation
	flags := uint32(
		windows.EXTENDED_STARTUPINFO_PRESENT |
			windows.CREATE_UNICODE_ENVIRONMENT,
	)
	// Do not use CREATE_NEW_PROCESS_GROUP here. Windows disables Ctrl+C for
	// the new group, which would turn ConPTY's ETX input into a forced Job
	// termination instead of the provider's normal graceful interrupt path.
	// The runner-owned Job Object already contains the complete child tree.
	if err := windows.CreateProcess(
		nil,
		commandLine,
		nil,
		nil,
		false,
		flags,
		&environmentBlock[0],
		currentDirectory,
		&startup.StartupInfo,
		&processInfo,
	); err != nil {
		return nil, fmt.Errorf("start ConPTY child: %w", err)
	}
	defer func() {
		_ = windows.CloseHandle(processInfo.Thread)
		if resultErr != nil {
			_ = windows.TerminateProcess(processInfo.Process, 1)
			_ = windows.CloseHandle(processInfo.Process)
		}
	}()
	// CreatePseudoConsole keeps its own references to these two channel ends.
	// Release the host copies after the child has been created so reads can
	// observe a broken channel when the pseudoconsole or child exits.
	closeWindowsHandles(ptyInput, ptyOutput)
	ptyInput, ptyOutput = 0, 0

	return &windowsConPTYProcess{
		pid:           int(processInfo.ProcessId),
		process:       processInfo.Process,
		job:           job,
		pseudoconsole: pseudoconsole,
		input:         os.NewFile(uintptr(inputWrite), "sessions-conpty-input"),
		output:        os.NewFile(uintptr(outputRead), "sessions-conpty-output"),
	}, nil
}

func startWindowsHeadlessProcess(command *exec.Cmd) (_ childProcess, resultErr error) {
	output, writePipe, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = writePipe.Close()
		if resultErr != nil {
			_ = output.Close()
		}
	}()
	command.Stdin = nil
	command.Stdout = writePipe
	command.Stderr = writePipe
	command.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
		HideWindow:    true,
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	processHandle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE,
		false,
		uint32(command.Process.Pid),
	)
	if err != nil {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
		return nil, fmt.Errorf("open headless child for job assignment: %w", err)
	}
	job, err := createWindowsRunnerJob(processHandle)
	_ = windows.CloseHandle(processHandle)
	if err != nil {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
		return nil, err
	}
	return &windowsHeadlessProcess{command: command, job: job, output: output}, nil
}

func createWindowsRunnerJob(process windows.Handle) (windows.Handle, error) {
	job, err := newWindowsRunnerJob()
	if err != nil {
		return 0, err
	}
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("assign child to runner Job Object: %w", err)
	}
	return job, nil
}

func newWindowsRunnerJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create runner Job Object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("configure runner Job Object: %w", err)
	}
	return job, nil
}

func (p *windowsConPTYProcess) PID() int {
	return p.pid
}

func (p *windowsConPTYProcess) Read(buffer []byte) (int, error) {
	return p.output.Read(buffer)
}

func (p *windowsConPTYProcess) Write(buffer []byte) (int, error) {
	return p.input.Write(buffer)
}

func (p *windowsConPTYProcess) Resize(cols, rows int) error {
	if cols < 1 || rows < 1 || cols > 32767 || rows > 32767 {
		return fmt.Errorf("invalid ConPTY size %dx%d", cols, rows)
	}
	return windows.ResizePseudoConsole(p.pseudoconsole, windows.Coord{
		X: int16(cols),
		Y: int16(rows),
	})
}

func (p *windowsConPTYProcess) RequestStop() error {
	p.stopOnce.Do(func() {
		// ConPTY delivers ETX through the provider's normal terminal Ctrl+C
		// path: raw-mode TUIs receive the byte and cooked consoles translate it
		// into a control event. Give the provider a bounded flush window, then
		// terminate the complete Job Object so no descendant is orphaned.
		if _, err := p.input.Write([]byte{3}); err != nil {
			p.stopErr = err
		}
		event, waitErr := windows.WaitForSingleObject(p.process, uint32(windowsStopGrace/time.Millisecond))
		if waitErr == nil && event == windows.WAIT_OBJECT_0 {
			return
		}
		if err := windows.TerminateJobObject(p.job, 130); err != nil && p.stopErr == nil {
			p.stopErr = err
		}
	})
	return p.stopErr
}

func (p *windowsConPTYProcess) ForceKill() error {
	return windows.TerminateJobObject(p.job, 1)
}

func (p *windowsConPTYProcess) CloseOutput() error {
	if p.output == nil {
		return nil
	}
	return p.output.Close()
}

func (p *windowsConPTYProcess) Wait(_ bool) exitInfo {
	p.waitOnce.Do(func() {
		event, err := windows.WaitForSingleObject(p.process, windows.INFINITE)
		if err != nil || event != windows.WAIT_OBJECT_0 {
			message := fmt.Sprintf("wait for ConPTY child: event=%d error=%v", event, err)
			p.waitInfo = exitInfo{Code: nil, Signal: &message}
			return
		}
		var exitCode uint32
		if err := windows.GetExitCodeProcess(p.process, &exitCode); err != nil {
			message := err.Error()
			p.waitInfo = exitInfo{Code: nil, Signal: &message}
			return
		}
		code := int(exitCode)
		p.waitInfo = exitInfo{Code: &code}
		windows.ClosePseudoConsole(p.pseudoconsole)
		p.pseudoconsole = 0
		closeWindowsHandles(p.process, p.job)
		p.process, p.job = 0, 0
		_ = p.input.Close()
	})
	return p.waitInfo
}

func (p *windowsHeadlessProcess) PID() int {
	return p.command.Process.Pid
}

func (p *windowsHeadlessProcess) Read(buffer []byte) (int, error) {
	return p.output.Read(buffer)
}

func (p *windowsHeadlessProcess) Write([]byte) (int, error) {
	return 0, errors.New("headless session does not accept terminal input")
}

func (p *windowsHeadlessProcess) Resize(int, int) error {
	return nil
}

func (p *windowsHeadlessProcess) RequestStop() error {
	p.stopOnce.Do(func() {
		// Headless children have no interactive console. The Job Object is the
		// truthful whole-tree termination boundary for this runner kind.
		p.stopErr = windows.TerminateJobObject(p.job, 130)
	})
	return p.stopErr
}

func (p *windowsHeadlessProcess) ForceKill() error {
	return windows.TerminateJobObject(p.job, 1)
}

func (p *windowsHeadlessProcess) CloseOutput() error {
	return p.output.Close()
}

func (p *windowsHeadlessProcess) Wait(_ bool) exitInfo {
	waitErr := p.command.Wait()
	if p.job != 0 {
		_ = windows.CloseHandle(p.job)
		p.job = 0
	}
	if p.command.ProcessState != nil {
		code := p.command.ProcessState.ExitCode()
		return exitInfo{Code: &code}
	}
	if waitErr != nil {
		message := waitErr.Error()
		return exitInfo{Code: nil, Signal: &message}
	}
	code := 0
	return exitInfo{Code: &code}
}

func windowsEnvironmentBlock(environment []string) ([]uint16, error) {
	sorted := append([]string(nil), environment...)
	sort.SliceStable(sorted, func(left, right int) bool {
		return strings.ToUpper(environmentKey(sorted[left])) < strings.ToUpper(environmentKey(sorted[right]))
	})
	block := make([]uint16, 0)
	for _, entry := range sorted {
		if strings.ContainsRune(entry, 0) {
			return nil, errors.New("child environment contains a NUL byte")
		}
		for _, character := range entry {
			block = utf16.AppendRune(block, character)
		}
		block = append(block, 0)
	}
	block = append(block, 0)
	if len(block) == 1 {
		block = append(block, 0)
	}
	return block, nil
}

func environmentKey(entry string) string {
	key, _, _ := strings.Cut(entry, "=")
	return key
}

func closeWindowsHandles(handles ...windows.Handle) {
	for _, handle := range handles {
		if handle != 0 && handle != windows.InvalidHandle {
			_ = windows.CloseHandle(handle)
		}
	}
}
