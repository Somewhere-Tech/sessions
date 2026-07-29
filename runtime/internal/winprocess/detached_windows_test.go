//go:build windows

package winprocess

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var detachedChildPIDFile = flag.String(
	"sessions-test-detached-child-pid-file",
	"",
	"internal helper path for the Windows Job breakaway test",
)

func TestDetachedChildSurvivesParentJobClose(t *testing.T) {
	if *detachedChildPIDFile != "" {
		t.Skip("parent assertion only")
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	jobOpen := true
	defer func() {
		if jobOpen {
			_ = windows.CloseHandle(job)
		}
	}()
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE |
		windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		t.Fatal(err)
	}

	pidFile := filepath.Join(t.TempDir(), "detached-child.pid")
	helperProcess, helperThread := startSuspendedJobHelper(t, pidFile)
	defer windows.CloseHandle(helperProcess)
	if err := windows.AssignProcessToJobObject(job, helperProcess); err != nil {
		_ = windows.TerminateProcess(helperProcess, 1)
		_ = windows.CloseHandle(helperThread)
		t.Fatalf("assign helper to kill-on-close parent Job: %v", err)
	}
	if _, err := windows.ResumeThread(helperThread); err != nil {
		_ = windows.TerminateProcess(helperProcess, 1)
		_ = windows.CloseHandle(helperThread)
		t.Fatalf("resume parent Job helper: %v", err)
	}
	if err := windows.CloseHandle(helperThread); err != nil {
		t.Fatal(err)
	}

	childPID := waitForDetachedChildPID(t, pidFile, helperProcess)
	child, err := windows.OpenProcess(
		windows.SYNCHRONIZE|windows.PROCESS_TERMINATE,
		false,
		childPID,
	)
	if err != nil {
		t.Fatalf("open detached child %d: %v", childPID, err)
	}
	defer windows.CloseHandle(child)
	defer func() {
		_ = windows.TerminateProcess(child, 0)
		_, _ = windows.WaitForSingleObject(child, 5_000)
	}()

	if err := windows.CloseHandle(job); err != nil {
		t.Fatal(err)
	}
	jobOpen = false
	event, err := windows.WaitForSingleObject(helperProcess, 5_000)
	if err != nil {
		t.Fatal(err)
	}
	if event != windows.WAIT_OBJECT_0 {
		t.Fatalf("parent Job helper survived Job close: wait event %d", event)
	}
	event, err = windows.WaitForSingleObject(child, 0)
	if err != nil {
		t.Fatal(err)
	}
	if event != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("detached child ended with its parent Job: wait event %d", event)
	}
}

func TestDetachedChildJobHelper(t *testing.T) {
	if *detachedChildPIDFile == "" {
		t.Skip("helper is launched only by TestDetachedChildSurvivesParentJobClose")
	}
	command := exec.Command("ping.exe", "-n", "60", "127.0.0.1")
	if err := StartDetached(command); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		*detachedChildPIDFile,
		[]byte(strconv.Itoa(command.Process.Pid)),
		0o600,
	); err != nil {
		_ = command.Process.Kill()
		t.Fatal(err)
	}
	select {}
}

func startSuspendedJobHelper(t *testing.T, pidFile string) (windows.Handle, windows.Handle) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine([]string{
		executable,
		"-test.run=^TestDetachedChildJobHelper$",
		"-test.v",
		"-sessions-test-detached-child-pid-file=" + pidFile,
	}))
	if err != nil {
		t.Fatal(err)
	}
	startup := windows.StartupInfo{
		Cb: uint32(unsafe.Sizeof(windows.StartupInfo{})),
	}
	var processInfo windows.ProcessInformation
	if err := windows.CreateProcess(
		nil,
		commandLine,
		nil,
		nil,
		false,
		windows.CREATE_SUSPENDED|windows.CREATE_NEW_PROCESS_GROUP|windows.CREATE_NO_WINDOW,
		nil,
		nil,
		&startup,
		&processInfo,
	); err != nil {
		t.Fatal(err)
	}
	return processInfo.Process, processInfo.Thread
}

func waitForDetachedChildPID(
	t *testing.T,
	pidFile string,
	helperProcess windows.Handle,
) uint32 {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		encoded, err := os.ReadFile(pidFile)
		if err == nil {
			pid, parseErr := strconv.ParseUint(strings.TrimSpace(string(encoded)), 10, 32)
			if parseErr != nil {
				t.Fatalf("parse detached child PID: %v", parseErr)
			}
			return uint32(pid)
		}
		event, waitErr := windows.WaitForSingleObject(helperProcess, 0)
		if waitErr != nil {
			t.Fatal(waitErr)
		}
		if event == windows.WAIT_OBJECT_0 {
			t.Fatalf("parent Job helper exited before writing %s", pidFile)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("parent Job helper did not write %s", pidFile)
	return 0
}
