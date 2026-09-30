package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
)

// DetachedLauncher starts a runner as an independent process on every platform
// that has no launchd: Linux, the BSDs, and a macOS container where launchctl
// is absent.
//
// The product's promise is that a session outlives the thing that started it,
// so the runner must not be a child the daemon can take with it. It is put in
// its own session and process group (setsid on Unix, a new process group on
// Windows), its stdio goes to the same <id>.log launchd would have written,
// and its environment is the same map the plist would have carried. What makes
// a runner re-adoptable is unchanged: its socket and its metadata in the runner
// state directory, which is how Attach finds it after the daemon restarts.
//
// What it deliberately does not do is supervise. launchd restarts a runner
// after a crash within the same boot and again at login; nothing here does,
// and the docs say so rather than implying a supervision this cannot provide.
type DetachedLauncher struct {
	config Config

	mu sync.Mutex
	// started is the pid this daemon process started for a session id. It is
	// forgotten as soon as the process is seen to exit, so a reaped pid the
	// kernel has handed to something else can never be mistaken for a runner.
	started map[string]int
	// exited is how a runner ended, for a launch that is still waiting on its
	// socket. Cleared when the same session starts again.
	exited map[string]string
}

func NewDetachedLauncher(config Config) *DetachedLauncher {
	return &DetachedLauncher{config: config, started: make(map[string]int)}
}

func (l *DetachedLauncher) ProgramArguments(proto.LaunchRequest) []string {
	if !isExecutableFile(l.config.RunnerPath) {
		return nil
	}
	return []string{l.config.RunnerPath}
}

// launchSpec is what this launcher must remember to be able to start the same
// runner again. launchd keeps the equivalent in the plist; without a plist the
// only durable copy is this sidecar, which is what lets Wake restart a session
// that a reboot stopped, and lets a later daemon incarnation say which process
// belonged to which session.
type launchSpec struct {
	ID      string            `json:"id"`
	Program []string          `json:"program"`
	Cwd     string            `json:"cwd"`
	Env     map[string]string `json:"env"`
	PID     int               `json:"pid,omitempty"`
	Started int64             `json:"started_at_ms,omitempty"`
}

func (l *DetachedLauncher) Prepare(request proto.LaunchRequest) error {
	paths := For(l.config.RunnerStateDir, request.Info.ID)
	bootID, err := CurrentBootID()
	if err != nil {
		return fmt.Errorf("prepare runner restart policy: %w", err)
	}
	if err := WriteRestartPermit(paths.KeepAlive, bootID); err != nil {
		return fmt.Errorf("prepare runner restart permit: %w", err)
	}
	if request.Env == nil {
		request.Env = make(map[string]string)
	}
	request.Env["RUNNER_RESTART_POLICY"] = "boot-scoped"
	program := l.ProgramArguments(request)
	if len(program) == 0 {
		_ = os.Remove(paths.KeepAlive)
		return fmt.Errorf("%s: the Sessions runner %q is not an executable file", l.describe(), l.config.RunnerPath)
	}
	if err := writeLaunchSpec(paths.Launch, launchSpec{
		ID: request.Info.ID, Program: program, Cwd: request.Info.Cwd, Env: request.Env,
	}); err != nil {
		_ = os.Remove(paths.KeepAlive)
		return err
	}
	return nil
}

// describe names the launcher and the platform. Every refusal below carries it,
// because the failure this launcher exists to end was a bare
// `exec: "launchctl": executable file not found in $PATH` that said nothing
// about which component had assumed a macOS-only supervisor.
func (l *DetachedLauncher) describe() string {
	return "the detached Sessions launcher on " + runtime.GOOS
}

func (l *DetachedLauncher) Preflight(request proto.LaunchRequest) error {
	if !isExecutableFile(l.config.RunnerPath) {
		return fmt.Errorf(
			"%s cannot start a session: the Sessions runner %q is missing or not executable; reinstall Sessions or set SESSIONS_RUNNER to the sessions-runner binary",
			l.describe(), l.config.RunnerPath,
		)
	}
	if err := runnableCwd(request.Info.Cwd); err != nil {
		return err
	}
	if _, ok := runnerCommandPath(request.Info.Cmd, request.Info.Cwd, request.Env["PATH"]); !ok {
		return fmt.Errorf(
			"session command %q is not executable in the Sessions runner PATH used by %s; install it under ~/.local/bin or /usr/local/bin, or choose another agent",
			request.Info.Cmd, l.describe(),
		)
	}
	return nil
}

func (l *DetachedLauncher) Launch(ctx context.Context, request proto.LaunchRequest) (proto.Runner, error) {
	if err := l.Preflight(request); err != nil {
		return nil, err
	}
	paths := For(l.config.RunnerStateDir, request.Info.ID)
	spec := launchSpec{
		ID: request.Info.ID, Program: l.ProgramArguments(request),
		Cwd: request.Info.Cwd, Env: request.Env,
	}
	if err := l.start(request.Info.ID, spec); err != nil {
		return nil, err
	}
	return waitForRunner(ctx, func() (proto.Runner, error) {
		return l.Attach(ctx, request.Info)
	}, paths.Socket, l.waitOptions(request.Info.ID))
}

// start runs one runner detached from this daemon and records its pid.
//
// The Wait goroutine does not tie the runner's life to the daemon's: it only
// collects the exit status so a finished runner cannot linger as a zombie
// child, and it forgets the pid at the same moment, which is what makes Reap
// unable to signal a number the kernel has since reused.
func (l *DetachedLauncher) start(id string, spec launchSpec) error {
	if len(spec.Program) == 0 {
		return fmt.Errorf("%s: no runner program to start for session %s", l.describe(), id)
	}
	logFile, err := openRunnerLog(l.config.RunnerStateDir, id)
	if err != nil {
		return err
	}
	defer logFile.Close()
	command := exec.Command(spec.Program[0], spec.Program[1:]...)
	command.Dir = spec.Cwd
	command.Env = runnerEnvironment(spec.Env)
	command.Stdin = nil
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = detachedSysProcAttr()
	if err := command.Start(); err != nil {
		return fmt.Errorf("%s could not start the runner for session %s: %w", l.describe(), id, err)
	}
	pid := command.Process.Pid
	l.remember(id, pid)
	spec.PID = pid
	spec.Started = time.Now().UnixMilli()
	if err := writeLaunchSpec(For(l.config.RunnerStateDir, id).Launch, spec); err != nil {
		return err
	}
	go func() {
		err := command.Wait()
		l.recordExit(id, err)
		l.forgetPID(id, pid)
	}()
	return nil
}

// waitOptions lets the wait end the moment the process does. This launcher
// started the runner, so "it exited" is a fact it holds rather than something
// to infer from sixty seconds of silence.
func (l *DetachedLauncher) waitOptions(id string) waitOptions {
	return waitOptions{
		RunnerStateDir: l.config.RunnerStateDir, ID: id,
		Stopped: func() (string, bool) { return l.exitOf(id) },
	}
}

func (l *DetachedLauncher) exitOf(id string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	reason, ok := l.exited[id]
	return reason, ok
}

func (l *DetachedLauncher) Attach(ctx context.Context, info proto.RunnerInfo) (proto.Runner, error) {
	if info.SocketPath == "" {
		info.SocketPath = For(l.config.RunnerStateDir, info.ID).Socket
	}
	return proto.DialRunner(ctx, info.SocketPath)
}

// Wake restarts a session that stayed stopped, from the launch sidecar written
// when it was first started. Without launchd there is nothing to kickstart, so
// waking is starting the same program again with the same environment — which
// is also why a session with no sidecar is refused by name rather than started
// with a guess at what it was.
func (l *DetachedLauncher) Wake(ctx context.Context, id string) (proto.Runner, error) {
	paths := For(l.config.RunnerStateDir, id)
	pending, pendingErr := ReadRestorePending(paths.RestorePending)
	if pendingErr != nil {
		return nil, fmt.Errorf("session %s is not paused after a reboot: %w", id, pendingErr)
	}
	spec, err := readLaunchSpec(paths.Launch)
	if err != nil {
		return nil, fmt.Errorf(
			"%s cannot restart session %s in place: no launch record at %s; resume it with `sessions resume %s`",
			l.describe(), id, paths.Launch, id,
		)
	}
	bootID, err := CurrentBootID()
	if err != nil {
		return nil, err
	}
	if err := WriteRestartPermit(paths.KeepAlive, bootID); err != nil {
		return nil, fmt.Errorf("renew runner permit: %w", err)
	}
	_ = os.Remove(paths.RestorePending)
	runner, err := l.wakeFrom(ctx, id, spec, paths)
	if err != nil {
		_ = os.Remove(paths.KeepAlive)
		_ = WriteRestorePending(paths.RestorePending, id, pending.Reason+" (last wake attempt: "+err.Error()+")")
		return nil, err
	}
	return runner, nil
}

func (l *DetachedLauncher) wakeFrom(
	ctx context.Context, id string, spec launchSpec, paths Paths,
) (proto.Runner, error) {
	if err := l.start(id, spec); err != nil {
		return nil, err
	}
	return waitForRunner(ctx, func() (proto.Runner, error) {
		return l.Attach(ctx, proto.RunnerInfo{ID: id, SocketPath: paths.Socket})
	}, paths.Socket, l.waitOptions(id))
}

// Reap ends the runner this daemon started for id and removes the files that
// describe a launch. Registry calls it after a failed launch, so a runner that
// never published its socket cannot keep working invisibly, and after a clean
// exit, where there is nothing left to signal.
//
// It signals the process group rather than the process: the runner owns a
// provider process and whatever that spawned, and the group is what setsid
// made it the leader of. It signals only a pid this daemon started and has not
// seen exit, because a pid alone is not an identity.
func (l *DetachedLauncher) Reap(id string) error {
	paths := For(l.config.RunnerStateDir, id)
	var reapErrors []error
	if pid, ok := l.forget(id); ok {
		if err := terminateRunnerProcess(pid); err != nil {
			reapErrors = append(reapErrors, fmt.Errorf("stop runner %s (pid %d): %w", id, pid, err))
		}
	}
	for _, path := range []string{paths.KeepAlive, paths.RestorePending, paths.Launch} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			reapErrors = append(reapErrors, err)
		}
	}
	return errors.Join(reapErrors...)
}

// recordExit remembers how a runner ended, so a launch still waiting for its
// socket can stop and say so instead of waiting out the deadline.
func (l *DetachedLauncher) recordExit(id string, err error) {
	reason := "exited"
	if err != nil {
		reason = "exited (" + err.Error() + ")"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.exited == nil {
		l.exited = make(map[string]string)
	}
	l.exited[id] = reason
}

func (l *DetachedLauncher) remember(id string, pid int) {
	if pid <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.started == nil {
		l.started = make(map[string]int)
	}
	l.started[id] = pid
	delete(l.exited, id)
}

func (l *DetachedLauncher) forget(id string) (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	pid, ok := l.started[id]
	delete(l.started, id)
	return pid, ok
}

// forgetPID drops the record only if it still names this exact process, so a
// relaunch that happened while the old one was exiting keeps its own pid.
func (l *DetachedLauncher) forgetPID(id string, pid int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.started[id] == pid {
		delete(l.started, id)
	}
}

func writeLaunchSpec(path string, spec launchSpec) error {
	encoded, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("encode runner launch record: %w", err)
	}
	if err := EnsureDir(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return fmt.Errorf("write runner launch record %s: %w", path, err)
	}
	return nil
}

func readLaunchSpec(path string) (launchSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return launchSpec{}, err
	}
	var spec launchSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return launchSpec{}, fmt.Errorf("decode runner launch record %s: %w", path, err)
	}
	if len(spec.Program) == 0 {
		return launchSpec{}, fmt.Errorf("runner launch record %s names no program", path)
	}
	return spec, nil
}

// runnerEnvironment is the launch environment as a process environment. It is
// the same map the plist carries, sorted so two launches of the same session
// produce the same environment in the same order.
func runnerEnvironment(environment map[string]string) []string {
	result := make([]string, 0, len(environment))
	for key, value := range environment {
		if key != "" {
			result = append(result, key+"="+value)
		}
	}
	sort.Strings(result)
	return result
}
