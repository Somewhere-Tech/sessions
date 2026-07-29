//go:build windows

package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const (
	windowsConPTYHelperEnv     = "SESSIONS_TEST_CONPTY_HELPER"
	windowsConPTYHelperPIDFile = "SESSIONS_TEST_CONPTY_DESCENDANT_PID_FILE"
	windowsConPTYHelperCols    = "SESSIONS_TEST_CONPTY_RESIZED_COLS"
	windowsConPTYHelperRows    = "SESSIONS_TEST_CONPTY_RESIZED_ROWS"
	windowsConPTYHelperDiag    = "SESSIONS_TEST_CONPTY_DIAGNOSTIC_FILE"
)

type synchronizedConPTYOutput struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func TestMain(m *testing.M) {
	if os.Getenv(windowsConPTYHelperEnv) == "1" {
		if err := runWindowsConPTYContractHelper(); err != nil {
			appendConPTYDiagnostic("error: " + err.Error())
			_, _ = fmt.Fprintf(os.Stderr, "CONPTY_HELPER_ERROR:%v\r\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func (o *synchronizedConPTYOutput) Write(data []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buffer.Write(data)
}

func (o *synchronizedConPTYOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buffer.String()
}

func TestWindowsConPTYContract(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	descendantPIDFile := filepath.Join(t.TempDir(), "descendant.pid")
	diagnosticFile := filepath.Join(t.TempDir(), "conpty-diagnostic.log")
	command := exec.Command(
		executable,
		"-test.run=^$",
	)
	command.Env = append(
		os.Environ(),
		windowsConPTYHelperEnv+"=1",
		windowsConPTYHelperPIDFile+"="+descendantPIDFile,
		windowsConPTYHelperCols+"=111",
		windowsConPTYHelperRows+"=37",
		windowsConPTYHelperDiag+"="+diagnosticFile,
	)

	child, err := startWindowsConPTYProcess(command, 80, 25)
	if err != nil {
		t.Fatal(err)
	}
	process := child.(*windowsConPTYProcess)
	var output synchronizedConPTYOutput
	outputDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(&output, process)
		outputDone <- copyErr
	}()
	waited := false
	outputDrained := false
	defer func() {
		if !waited {
			_ = process.ForceKill()
			_ = process.Wait(false)
		}
		if !outputDrained {
			select {
			case <-outputDone:
			case <-time.After(5 * time.Second):
			}
		}
	}()

	waitForConPTYMarker(
		t,
		&output,
		outputDone,
		process,
		diagnosticFile,
		"READY:80x25 UNICODE:雪🙂مرحبا",
	)
	descendantPID := waitForConPTYHelperPID(t, descendantPIDFile)
	descendant, err := windows.OpenProcess(
		windows.SYNCHRONIZE|windows.PROCESS_TERMINATE,
		false,
		descendantPID,
	)
	if err != nil {
		t.Fatalf("open ConPTY helper descendant %d: %v", descendantPID, err)
	}
	defer windows.CloseHandle(descendant)
	event, err := windows.WaitForSingleObject(descendant, 0)
	if err != nil {
		t.Fatalf("probe ConPTY helper descendant: %v", err)
	}
	if event != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("ConPTY helper descendant %d exited before ETX: event=%d", descendantPID, event)
	}
	defer func() {
		event, waitErr := windows.WaitForSingleObject(descendant, 0)
		if waitErr == nil && event == uint32(windows.WAIT_TIMEOUT) {
			_ = windows.TerminateProcess(descendant, 1)
		}
	}()

	if err := process.Resize(111, 37); err != nil {
		t.Fatalf("resize production ConPTY: %v", err)
	}
	const input = "INPUT:résumé-雪🙂"
	if _, err := process.Write([]byte(input + "\r")); err != nil {
		t.Fatalf("write Unicode input to production ConPTY: %v", err)
	}
	waitForConPTYMarker(t, &output, outputDone, process, diagnosticFile, input+" RESIZED:111x37")

	if err := process.RequestStop(); err != nil {
		t.Fatalf("request graceful ConPTY stop: %v\noutput:\n%s", err, output.String())
	}
	info := process.Wait(false)
	waited = true
	select {
	case copyErr := <-outputDone:
		outputDrained = true
		if copyErr != nil && !errors.Is(copyErr, os.ErrClosed) {
			t.Fatalf("read production ConPTY output: %v", copyErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("production ConPTY output did not close\noutput:\n%s", output.String())
	}
	if info.Code == nil || *info.Code != 0 || info.Signal != nil {
		t.Fatalf("graceful ConPTY exit = %#v\noutput:\n%s", info, output.String())
	}
	if !strings.Contains(output.String(), "FINAL:ETX") {
		t.Fatalf("graceful ETX final marker missing\noutput:\n%s", output.String())
	}

	event, err = windows.WaitForSingleObject(descendant, 5_000)
	if err != nil {
		t.Fatalf("wait for runner-owned descendant: %v", err)
	}
	if event != windows.WAIT_OBJECT_0 {
		t.Fatalf(
			"runner-owned descendant %d survived ConPTY Job close: event=%d\noutput:\n%s",
			descendantPID,
			event,
			output.String(),
		)
	}
}

func runWindowsConPTYContractHelper() error {
	appendConPTYDiagnostic("helper entered")
	descendant := exec.Command("ping.exe", "-n", "120", "127.0.0.1")
	descendant.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
		HideWindow:    true,
	}
	if err := descendant.Start(); err != nil {
		return fmt.Errorf("start ConPTY descendant: %w", err)
	}
	appendConPTYDiagnostic(fmt.Sprintf("descendant started: %d", descendant.Process.Pid))
	pidFile := os.Getenv(windowsConPTYHelperPIDFile)
	if pidFile == "" {
		return errors.New("ConPTY descendant PID file is required")
	}
	if err := os.WriteFile(
		pidFile,
		[]byte(strconv.Itoa(descendant.Process.Pid)),
		0o600,
	); err != nil {
		return fmt.Errorf("write ConPTY descendant PID: %w", err)
	}
	appendConPTYDiagnostic("descendant PID written")

	cols, rows, err := windowsConsoleSize()
	if err != nil {
		return err
	}
	appendConPTYDiagnostic(fmt.Sprintf("console size: %dx%d", cols, rows))
	fmt.Printf("READY:%dx%d UNICODE:雪🙂مرحبا\r\n", cols, rows)
	_ = os.Stdout.Sync()
	appendConPTYDiagnostic("ready marker written")

	lines := make(chan string, 1)
	scanErrors := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		if scanner.Scan() {
			lines <- scanner.Text()
			return
		}
		scanErrors <- scanner.Err()
	}()
	var input string
	select {
	case input = <-lines:
	case scanErr := <-scanErrors:
		if scanErr == nil {
			return errors.New("read ConPTY Unicode input: terminal closed")
		}
		return fmt.Errorf("read ConPTY Unicode input: %w", scanErr)
	case <-time.After(10 * time.Second):
		return errors.New("timed out waiting for ConPTY Unicode input")
	}

	wantCols, err := conPTYHelperDimension(windowsConPTYHelperCols)
	if err != nil {
		return err
	}
	wantRows, err := conPTYHelperDimension(windowsConPTYHelperRows)
	if err != nil {
		return err
	}
	cols, rows, err = waitForWindowsConsoleSize(wantCols, wantRows, 5*time.Second)
	if err != nil {
		return err
	}
	fmt.Printf("%s RESIZED:%dx%d\r\n", input, cols, rows)
	_ = os.Stdout.Sync()

	if err := setConPTYRawInput(); err != nil {
		return err
	}
	stopInput := make(chan byte, 1)
	stopErrors := make(chan error, 1)
	go func() {
		buffer := make([]byte, 1)
		if _, err := os.Stdin.Read(buffer); err != nil {
			stopErrors <- err
			return
		}
		stopInput <- buffer[0]
	}()
	select {
	case inputByte := <-stopInput:
		if inputByte != 3 {
			return fmt.Errorf("received terminal stop byte %d; want ETX 3", inputByte)
		}
		fmt.Print("FINAL:ETX\r\n")
		_ = os.Stdout.Sync()
		return nil
	case stopErr := <-stopErrors:
		return fmt.Errorf("read ConPTY ETX: %w", stopErr)
	case <-time.After(10 * time.Second):
		return errors.New("timed out waiting for ConPTY ETX")
	}
}

func setConPTYRawInput() error {
	input, err := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	if err != nil {
		return fmt.Errorf("get ConPTY console input handle: %w", err)
	}
	var mode uint32
	if err := windows.GetConsoleMode(input, &mode); err != nil {
		return fmt.Errorf("query ConPTY console input mode: %w", err)
	}
	mode &^= windows.ENABLE_PROCESSED_INPUT | windows.ENABLE_LINE_INPUT | windows.ENABLE_ECHO_INPUT
	if err := windows.SetConsoleMode(input, mode); err != nil {
		return fmt.Errorf("set ConPTY raw input mode: %w", err)
	}
	appendConPTYDiagnostic(fmt.Sprintf("raw input mode: 0x%x", mode))
	return nil
}

func waitForConPTYMarker(
	t *testing.T,
	output *synchronizedConPTYOutput,
	outputDone <-chan error,
	process *windowsConPTYProcess,
	diagnosticFile string,
	marker string,
) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if strings.Contains(output.String(), marker) {
			return
		}
		select {
		case copyErr := <-outputDone:
			t.Fatalf(
				"production ConPTY output closed before %q: %v\n%s\noutput:\n%s",
				marker,
				copyErr,
				conPTYFailureDetails(process, diagnosticFile),
				output.String(),
			)
		case <-deadline.C:
			t.Fatalf(
				"timed out waiting for %q\n%s\noutput:\n%s",
				marker,
				conPTYFailureDetails(process, diagnosticFile),
				output.String(),
			)
		case <-ticker.C:
		}
	}
}

func appendConPTYDiagnostic(message string) {
	path := os.Getenv(windowsConPTYHelperDiag)
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = fmt.Fprintln(file, message)
}

func conPTYFailureDetails(process *windowsConPTYProcess, diagnosticFile string) string {
	var details strings.Builder
	event, waitErr := windows.WaitForSingleObject(process.process, 0)
	fmt.Fprintf(&details, "process wait event=%d error=%v", event, waitErr)
	if waitErr == nil && event == windows.WAIT_OBJECT_0 {
		var code uint32
		if err := windows.GetExitCodeProcess(process.process, &code); err == nil {
			fmt.Fprintf(&details, " exit=%d", code)
		}
	}
	diagnostic, err := os.ReadFile(diagnosticFile)
	if err != nil {
		fmt.Fprintf(&details, "\nhelper diagnostics unavailable: %v", err)
	} else {
		fmt.Fprintf(&details, "\nhelper diagnostics:\n%s", diagnostic)
	}
	return details.String()
}

func waitForConPTYHelperPID(t *testing.T, path string) uint32 {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		encoded, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.ParseUint(strings.TrimSpace(string(encoded)), 10, 32)
			if parseErr != nil {
				t.Fatalf("parse ConPTY helper descendant PID: %v", parseErr)
			}
			return uint32(pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("ConPTY helper did not write %s", path)
	return 0
}

func windowsConsoleSize() (int, int, error) {
	output, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		return 0, 0, fmt.Errorf("get ConPTY console output handle: %w", err)
	}
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(output, &info); err != nil {
		return 0, 0, fmt.Errorf("query ConPTY console size: %w", err)
	}
	return int(info.Window.Right-info.Window.Left) + 1,
		int(info.Window.Bottom-info.Window.Top) + 1,
		nil
}

func waitForWindowsConsoleSize(wantCols, wantRows int, timeout time.Duration) (int, int, error) {
	deadline := time.Now().Add(timeout)
	var cols, rows int
	var err error
	for time.Now().Before(deadline) {
		cols, rows, err = windowsConsoleSize()
		if err == nil && cols == wantCols && rows == wantRows {
			return cols, rows, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		return 0, 0, err
	}
	return 0, 0, fmt.Errorf(
		"ConPTY size stayed %dx%d; want %dx%d",
		cols,
		rows,
		wantCols,
		wantRows,
	)
}

func conPTYHelperDimension(name string) (int, error) {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value < 1 {
		return 0, fmt.Errorf("invalid %s=%q", name, os.Getenv(name))
	}
	return value, nil
}
