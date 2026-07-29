//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/windows"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/winprocess"
)

const (
	supervisorMutexName     = "Local\\SomewhereSessionsSupervisor"
	supervisorStopEventName = "Local\\SomewhereSessionsSupervisorStop"
	supervisorStopEventEnv  = "SESSIONS_SUPERVISOR_STOP_EVENT"
)

type supervisorLoopOptions struct {
	stableLifetime time.Duration
	initialBackoff time.Duration
	maximumBackoff time.Duration
	stopTimeout    time.Duration
}

var defaultSupervisorLoopOptions = supervisorLoopOptions{
	stableLifetime: 30 * time.Second,
	initialBackoff: time.Second,
	maximumBackoff: 30 * time.Second,
	stopTimeout:    10 * time.Second,
}

func runPlatformSupervisor(arguments []string) (bool, error) {
	if len(arguments) == 0 {
		return false, nil
	}
	if arguments[0] == "--supervise-stop" {
		return true, signalSupervisorStop(supervisorStopEventName)
	}
	if arguments[0] != "--supervise" {
		return false, nil
	}
	for index := 1; index < len(arguments); index++ {
		if index+1 >= len(arguments) {
			return true, fmt.Errorf("sessionsd supervisor: missing value for %s", arguments[index])
		}
		switch arguments[index] {
		case "--port":
			if _, err := strconv.Atoi(arguments[index+1]); err != nil {
				return true, fmt.Errorf("sessionsd supervisor: invalid port %q", arguments[index+1])
			}
			_ = os.Setenv("SESSIONS_PORT", arguments[index+1])
		case "--runner":
			_ = os.Setenv("SESSIONS_RUNNER", arguments[index+1])
		default:
			return true, fmt.Errorf("sessionsd supervisor: unknown option %s", arguments[index])
		}
		index++
	}
	mutex, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(supervisorMutexName))
	if err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			if mutex != 0 {
				_ = windows.CloseHandle(mutex)
			}
			return true, nil
		}
		return true, fmt.Errorf("sessionsd supervisor mutex: %w", err)
	}
	defer windows.CloseHandle(mutex)
	stopEvent, err := createSupervisorStopEvent(supervisorStopEventName)
	if err != nil {
		return true, err
	}
	defer windows.CloseHandle(stopEvent)
	_ = os.Setenv(supervisorStopEventEnv, supervisorStopEventName)

	config, err := state.ConfigFromEnv()
	if err != nil {
		return true, fmt.Errorf("sessionsd supervisor configuration: %w", err)
	}
	if err := writeSupervisorIdentity(config.StateRoot); err != nil {
		log.Printf("sessionsd supervisor identity: %v", err)
	}
	defer removeSupervisorIdentity(config.StateRoot)
	executable, err := os.Executable()
	if err != nil {
		return true, fmt.Errorf("sessionsd supervisor executable: %w", err)
	}
	stopRequested := waitForSupervisorStop(stopEvent)

	startDaemon := func() (*exec.Cmd, error) {
		command := exec.Command(executable, "--serve")
		command.Env = os.Environ()
		command.Stdin = nil
		command.Stdout = nil
		command.Stderr = nil
		if err := winprocess.StartDetached(command); err != nil {
			return nil, err
		}
		return command, nil
	}
	if err := superviseDaemon(stopRequested, startDaemon, defaultSupervisorLoopOptions); err != nil {
		return true, fmt.Errorf("sessionsd supervisor: %w", err)
	}
	return true, nil
}

// superviseDaemon owns only the current daemon process. Detached runners are
// deliberately absent from this loop, so daemon crash/backoff and idle
// supervisor shutdown cannot become runner lifecycle operations.
func superviseDaemon(
	stopRequested <-chan struct{},
	startDaemon func() (*exec.Cmd, error),
	options supervisorLoopOptions,
) error {
	backoff := options.initialBackoff
	for {
		started := time.Now()
		command, err := startDaemon()
		if err != nil {
			return fmt.Errorf("start daemon: %w", err)
		}
		exited := make(chan error, 1)
		go func() {
			exited <- command.Wait()
		}()

		var commandErr error
		select {
		case commandErr = <-exited:
		case <-stopRequested:
			log.Printf("sessionsd supervisor: idle handoff requested")
			select {
			case commandErr = <-exited:
			case <-time.After(options.stopTimeout):
				log.Printf(
					"sessionsd supervisor: daemon did not stop within %s; terminating only the idle daemon",
					options.stopTimeout,
				)
				_ = command.Process.Kill()
				commandErr = <-exited
			}
			if commandErr != nil {
				log.Printf("sessionsd supervisor: daemon stopped for handoff: %v", commandErr)
			}
			return nil
		}
		lifetime := time.Since(started)
		if commandErr != nil {
			log.Printf("sessionsd supervisor: daemon exited after %s: %v", lifetime.Round(time.Millisecond), commandErr)
		} else {
			log.Printf("sessionsd supervisor: daemon exited after %s", lifetime.Round(time.Millisecond))
		}
		if lifetime >= options.stableLifetime {
			backoff = options.initialBackoff
		} else if backoff < options.maximumBackoff {
			backoff *= 2
			if backoff > options.maximumBackoff {
				backoff = options.maximumBackoff
			}
		}
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-stopRequested:
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		}
	}
}

func createSupervisorStopEvent(name string) (windows.Handle, error) {
	handle, err := windows.CreateEvent(
		nil,
		1,
		0,
		windows.StringToUTF16Ptr(name),
	)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return 0, fmt.Errorf("sessionsd supervisor stop event %q already exists; refusing possible name squatting", name)
	}
	if err != nil {
		return 0, fmt.Errorf("sessionsd supervisor stop event: %w", err)
	}
	return handle, nil
}

func waitForSupervisorStop(handle windows.Handle) <-chan struct{} {
	stopped := make(chan struct{})
	go func() {
		event, err := windows.WaitForSingleObject(handle, windows.INFINITE)
		if err != nil {
			log.Printf("sessionsd supervisor: wait for stop event: %v", err)
			return
		}
		if event == windows.WAIT_OBJECT_0 {
			close(stopped)
		}
	}()
	return stopped
}

func signalSupervisorStop(name string) error {
	handle, err := windows.OpenEvent(
		windows.EVENT_MODIFY_STATE|windows.SYNCHRONIZE,
		false,
		windows.StringToUTF16Ptr(name),
	)
	if err != nil {
		return fmt.Errorf("sessionsd supervisor is not available for a safe handoff: %w", err)
	}
	if err := windows.SetEvent(handle); err != nil {
		_ = windows.CloseHandle(handle)
		return fmt.Errorf("signal sessionsd supervisor handoff: %w", err)
	}
	_ = windows.CloseHandle(handle)

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		probe, err := windows.OpenEvent(
			windows.SYNCHRONIZE,
			false,
			windows.StringToUTF16Ptr(name),
		)
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("confirm sessionsd supervisor handoff: %w", err)
		}
		_ = windows.CloseHandle(probe)
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("sessionsd supervisor did not finish the handoff within 15s")
}

func writeSupervisorIdentity(stateRoot string) error {
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(map[string]string{
		"pid":     strconv.Itoa(os.Getpid()),
		"version": version,
	}, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	path := filepath.Join(stateRoot, "supervisor.json")
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func removeSupervisorIdentity(stateRoot string) {
	if err := os.Remove(filepath.Join(stateRoot, "supervisor.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("sessionsd supervisor identity cleanup: %v", err)
	}
}
