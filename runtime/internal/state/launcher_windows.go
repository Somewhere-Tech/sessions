//go:build windows

package state

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"golang.org/x/sys/windows"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/winprocess"
)

// WindowsLauncher starts one detached per-user runner process. The runner,
// rather than the desktop viewer or sessionsd, owns the provider process and
// its Job Object. Consequently closing or upgrading the viewer does not end a
// session, and sessionsd can re-adopt the runner after a daemon restart.
type WindowsLauncher struct {
	config Config
	mu     sync.Mutex
	// started identifies the exact process this launcher started for a session
	// id. Windows reuses process ids, so the pid alone is not an identity; the
	// creation time captured while our own start handle still held the pid
	// makes the pair unforgeable by reuse.
	started map[string]launchedRunner
}

type launchedRunner struct {
	pid     uint32
	created windows.Filetime
}

func NewWindowsLauncher(config Config) *WindowsLauncher {
	return &WindowsLauncher{config: config, started: make(map[string]launchedRunner)}
}

func (l *WindowsLauncher) ProgramArguments(proto.LaunchRequest) []string {
	if !isExecutableFile(l.config.RunnerPath) {
		return nil
	}
	return []string{l.config.RunnerPath}
}

func (l *WindowsLauncher) Preflight(request proto.LaunchRequest) error {
	if _, ok := runnerCommandPath(request.Info.Cmd, request.Info.Cwd, request.Env["PATH"]); !ok {
		return fmt.Errorf(
			"session command %q is not executable in the Sessions runner PATH; install it for this Windows user or choose another agent",
			request.Info.Cmd,
		)
	}
	return nil
}

func (l *WindowsLauncher) Launch(ctx context.Context, request proto.LaunchRequest) (proto.Runner, error) {
	if err := l.Preflight(request); err != nil {
		return nil, err
	}
	// launchd points StandardOutPath/StandardErrorPath at <id>.log on macOS. A
	// Windows runner that dies before publishing its pipe otherwise leaves no
	// diagnostic at all, which is the one case where the user most needs to
	// know where their session went.
	logFile, err := openRunnerLog(l.config.RunnerStateDir, request.Info.ID)
	if err != nil {
		return nil, err
	}
	// The child inherits the handle; sessionsd keeps no writer of its own.
	defer logFile.Close()

	command := exec.Command(l.config.RunnerPath)
	command.Dir = request.Info.Cwd
	command.Env = windowsRunnerEnvironment(request.Env)
	command.Stdin = nil
	command.Stdout = logFile
	command.Stderr = logFile
	if err := winprocess.StartDetached(command); err != nil {
		return nil, fmt.Errorf("start Windows runner %s: %w", request.Info.ID, err)
	}
	// Identify the process while os/exec still owns its handle, so the pid
	// cannot have been recycled between start and identification.
	l.remember(request.Info.ID, command.Process.Pid)
	// A released os.Process leaves the child fully independent from sessionsd.
	// The runner's named local endpoint and durable metadata are the re-adoption
	// boundary; no viewer-owned process handle is retained.
	if err := command.Process.Release(); err != nil {
		return nil, fmt.Errorf("release Windows runner %s: %w", request.Info.ID, err)
	}
	return l.waitAndAttach(ctx, request.Info)
}

// openRunnerLog opens <RunnerStateDir>/<id>.log for append. A failure here is
// reported rather than swallowed: the same directory is where the runner must
// publish its metadata, so an unwritable one is a launch problem, not a
// logging preference.
func openRunnerLog(runnerStateDir, id string) (*os.File, error) {
	if err := EnsureDir(runnerStateDir); err != nil {
		return nil, fmt.Errorf("create runner state directory %s: %w", runnerStateDir, err)
	}
	path := filepath.Join(runnerStateDir, id+".log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open runner log %s: %w", path, err)
	}
	return file, nil
}

func (l *WindowsLauncher) remember(id string, pid int) {
	if pid <= 0 {
		return
	}
	created, ok := processCreationTime(uint32(pid))
	if !ok {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.started == nil {
		l.started = make(map[string]launchedRunner)
	}
	l.started[id] = launchedRunner{pid: uint32(pid), created: created}
}

func (l *WindowsLauncher) forget(id string) (launchedRunner, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	record, ok := l.started[id]
	delete(l.started, id)
	return record, ok
}

func processCreationTime(pid uint32) (windows.Filetime, bool) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return windows.Filetime{}, false
	}
	defer windows.CloseHandle(handle)
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return windows.Filetime{}, false
	}
	return creation, true
}

func (l *WindowsLauncher) Attach(ctx context.Context, info proto.RunnerInfo) (proto.Runner, error) {
	if info.SocketPath == "" {
		info.SocketPath = For(l.config.RunnerStateDir, info.ID).Socket
	}
	return proto.DialRunner(ctx, info.SocketPath)
}

func (l *WindowsLauncher) waitAndAttach(ctx context.Context, info proto.RunnerInfo) (proto.Runner, error) {
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for {
		runner, err := l.Attach(ctx, info)
		if err == nil {
			return runner, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("runner did not create its local endpoint within 60s: %s: %w", info.SocketPath, lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(30 * time.Millisecond):
		}
	}
}

// Reap ends the runner this launcher started for id. Registry calls it after a
// failed launch precisely so a runner that never published its endpoint cannot
// keep spinning invisibly, and again after a clean exit so the ledger can
// record the lane as reaped.
//
// It only ever terminates a process it can positively identify as that runner:
// the pid must be one this launcher started for this id, and the process now
// holding that pid must have the creation time recorded at start. Anything
// else — unknown id, already-exited process, recycled pid — is reported as
// nothing to reap rather than as a kill, because a wrong guess here would end
// an unrelated process belonging to the signed-in user.
//
// Runner metadata and <id>.log are deliberately left in place for diagnosis.
func (l *WindowsLauncher) Reap(id string) error {
	record, ok := l.forget(id)
	if !ok {
		return nil
	}
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE,
		false, record.pid,
	)
	if err != nil {
		// The process is gone, or this user may no longer touch it. Either way
		// there is nothing this launcher can honestly claim to have reaped.
		return nil
	}
	defer windows.CloseHandle(handle)
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return nil
	}
	if creation != record.created {
		// Windows handed this pid to something else after our runner exited.
		return nil
	}
	if err := windows.TerminateProcess(handle, 1); err != nil {
		// A process that already exited cannot be terminated; that is success.
		if event, waitErr := windows.WaitForSingleObject(handle, 0); waitErr == nil && event == windows.WAIT_OBJECT_0 {
			return nil
		}
		return fmt.Errorf("terminate Windows runner %s (pid %d): %w", id, record.pid, err)
	}
	if event, waitErr := windows.WaitForSingleObject(handle, 5_000); waitErr == nil && event != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("Windows runner %s (pid %d) did not exit after terminate", id, record.pid)
	}
	return nil
}

func windowsRunnerEnvironment(environment map[string]string) []string {
	result := make([]string, 0, len(environment))
	for key, value := range environment {
		if key != "" {
			result = append(result, key+"="+value)
		}
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left] < result[right]
	})
	return result
}

var _ proto.RunnerLauncher = (*WindowsLauncher)(nil)
var _ proto.RunnerLaunchPreflight = (*WindowsLauncher)(nil)
