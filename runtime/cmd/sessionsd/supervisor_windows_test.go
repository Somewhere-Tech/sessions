//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const supervisorProcessHelperEnv = "SESSIONS_TEST_SUPERVISOR_PROCESS_HELPER"

func TestSupervisorRestartsCrashedDaemonWithoutTouchingDetachedRunner(t *testing.T) {
	if os.Getenv(supervisorProcessHelperEnv) != "" {
		t.Skip("parent assertion only")
	}

	runner, err := startSupervisorProcessHelper("runner")
	if err != nil {
		t.Fatalf("start detached runner helper: %v", err)
	}
	t.Cleanup(func() {
		_ = runner.Process.Kill()
		_ = runner.Wait()
	})
	runnerPID := runner.Process.Pid
	if !processIsRunning(t, runnerPID) {
		t.Fatalf("detached runner helper %d did not remain alive after start", runnerPID)
	}

	startedDaemons := make(chan *exec.Cmd, 4)
	startDaemon := func() (*exec.Cmd, error) {
		command, startErr := startSupervisorProcessHelper("daemon")
		if startErr == nil {
			startedDaemons <- command
		}
		return command, startErr
	}
	stop := make(chan struct{})
	stopped := false
	stopSupervisor := func() {
		if !stopped {
			close(stop)
			stopped = true
		}
	}
	t.Cleanup(stopSupervisor)
	supervisorDone := make(chan error, 1)
	go func() {
		supervisorDone <- superviseDaemon(stop, startDaemon, supervisorLoopOptions{
			stableLifetime: 100 * time.Millisecond,
			initialBackoff: 5 * time.Millisecond,
			maximumBackoff: 20 * time.Millisecond,
			stopTimeout:    100 * time.Millisecond,
		})
	}()

	first := waitForStartedDaemon(t, startedDaemons)
	firstPID := first.Process.Pid
	if err := first.Process.Kill(); err != nil {
		t.Fatalf("force-terminate first supervised daemon helper %d: %v", firstPID, err)
	}
	second := waitForStartedDaemon(t, startedDaemons)
	secondPID := second.Process.Pid
	if secondPID == firstPID {
		t.Fatalf("supervisor reused crashed daemon PID %d", firstPID)
	}
	if !processIsRunning(t, secondPID) {
		t.Fatalf("replacement supervised daemon helper %d is not running", secondPID)
	}
	if runner.Process.Pid != runnerPID || !processIsRunning(t, runnerPID) {
		t.Fatalf(
			"detached runner helper changed across daemon crash: before=%d after=%d alive=%v",
			runnerPID,
			runner.Process.Pid,
			processIsRunning(t, runnerPID),
		)
	}

	stopSupervisor()
	select {
	case err := <-supervisorDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not stop after the scratch handoff")
	}
	if runner.Process.Pid != runnerPID || !processIsRunning(t, runnerPID) {
		t.Fatalf(
			"detached runner helper changed when supervisor stopped: before=%d after=%d alive=%v",
			runnerPID,
			runner.Process.Pid,
			processIsRunning(t, runnerPID),
		)
	}
}

func TestSupervisorProcessHelper(t *testing.T) {
	if os.Getenv(supervisorProcessHelperEnv) == "" {
		t.Skip("helper is launched only by the Windows supervisor process test")
	}
	for {
		time.Sleep(time.Hour)
	}
}

func startSupervisorProcessHelper(role string) (*exec.Cmd, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	command := exec.Command(
		executable,
		"-test.run=^TestSupervisorProcessHelper$",
		"-test.v",
	)
	command.Env = append(os.Environ(), supervisorProcessHelperEnv+"="+role)
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	// This test proves daemon replacement and runner independence, not parent
	// Job breakaway. That contract has its own controlled native test in
	// internal/winprocess. GitHub-hosted runners intentionally place this test
	// inside a restrictive outer Job that rejects CREATE_BREAKAWAY_FROM_JOB.
	command.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
		HideWindow:    true,
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	return command, nil
}

func waitForStartedDaemon(t *testing.T, started <-chan *exec.Cmd) *exec.Cmd {
	t.Helper()
	select {
	case command := <-started:
		return command
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not start the scratch daemon helper")
		return nil
	}
}

func processIsRunning(t *testing.T, pid int) bool {
	t.Helper()
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatalf("open scratch process %d: %v", pid, err)
	}
	defer windows.CloseHandle(handle)
	event, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		t.Fatalf("inspect scratch process %d: %v", pid, err)
	}
	return event == uint32(windows.WAIT_TIMEOUT)
}

func TestSupervisorStopEventSignalsAndDisappears(t *testing.T) {
	name := fmt.Sprintf(
		"Local\\SomewhereSessionsSupervisorStopTest-%d-%d",
		os.Getpid(),
		time.Now().UnixNano(),
	)
	handle, err := createSupervisorStopEvent(name)
	if err != nil {
		t.Fatal(err)
	}
	stopped := waitForSupervisorStop(handle)

	signaled := make(chan error, 1)
	go func() {
		signaled <- signalSupervisorStop(name)
	}()

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("stop event was not observed")
	}
	if err := windows.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-signaled:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stop signal did not observe event cleanup")
	}
}

func TestSupervisorStopEventRejectsExistingName(t *testing.T) {
	name := fmt.Sprintf(
		"Local\\SomewhereSessionsSupervisorStopSquatTest-%d-%d",
		os.Getpid(),
		time.Now().UnixNano(),
	)
	handle, err := createSupervisorStopEvent(name)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)

	if duplicate, err := createSupervisorStopEvent(name); err == nil {
		_ = windows.CloseHandle(duplicate)
		t.Fatal("expected an existing stop-event name to be rejected")
	}
}

func TestPlatformStopEventReachesDaemonSignalChannel(t *testing.T) {
	name := fmt.Sprintf(
		"Local\\SomewhereSessionsDaemonStopTest-%d-%d",
		os.Getpid(),
		time.Now().UnixNano(),
	)
	handle, err := createSupervisorStopEvent(name)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)

	t.Setenv(supervisorStopEventEnv, name)
	stop := make(chan os.Signal, 1)
	cleanup := watchPlatformStop(stop)
	defer cleanup()
	if err := windows.SetEvent(handle); err != nil {
		t.Fatal(err)
	}
	select {
	case signal := <-stop:
		if signal.String() != "per-user supervisor handoff" {
			t.Fatalf("unexpected signal %q", signal.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not observe the supervisor stop event")
	}
}
