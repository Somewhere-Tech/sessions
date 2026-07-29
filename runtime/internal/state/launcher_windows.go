//go:build windows

package state

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/winprocess"
)

// WindowsLauncher starts one detached per-user runner process. The runner,
// rather than the desktop viewer or sessionsd, owns the provider process and
// its Job Object. Consequently closing or upgrading the viewer does not end a
// session, and sessionsd can re-adopt the runner after a daemon restart.
type WindowsLauncher struct {
	config Config
}

func NewWindowsLauncher(config Config) *WindowsLauncher {
	return &WindowsLauncher{config: config}
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
	command := exec.Command(l.config.RunnerPath)
	command.Dir = request.Info.Cwd
	command.Env = windowsRunnerEnvironment(request.Env)
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	if err := winprocess.StartDetached(command); err != nil {
		return nil, fmt.Errorf("start Windows runner %s: %w", request.Info.ID, err)
	}
	// A released os.Process leaves the child fully independent from sessionsd.
	// The runner's named local endpoint and durable metadata are the re-adoption
	// boundary; no viewer-owned process handle is retained.
	if err := command.Process.Release(); err != nil {
		return nil, fmt.Errorf("release Windows runner %s: %w", request.Info.ID, err)
	}
	return l.waitAndAttach(ctx, request.Info)
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

func (l *WindowsLauncher) Reap(string) error {
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
