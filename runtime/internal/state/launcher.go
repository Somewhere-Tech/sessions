package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
)

// LaunchdLauncher boots the plist already written by Registry and then
// attaches through the canonical runner socket protocol.
type LaunchdLauncher struct {
	config Config
}

func NewLaunchdLauncher(config Config) *LaunchdLauncher {
	return &LaunchdLauncher{config: config}
}

func (l *LaunchdLauncher) ProgramArguments(proto.LaunchRequest) []string {
	if !isExecutableFile(l.config.RunnerPath) {
		return nil
	}
	return []string{l.config.RunnerPath}
}

func (l *LaunchdLauncher) Prepare(request proto.LaunchRequest) error {
	_, err := writePlist(l.config.LaunchAgentsDir, plistArgs{
		ID:               request.Info.ID,
		ProgramArguments: l.ProgramArguments(request),
		Env:              request.Env,
		Cwd:              request.Info.Cwd,
		LogPath:          filepath.Join(l.config.RunnerStateDir, request.Info.ID+".log"),
	})
	return err
}

func (l *LaunchdLauncher) Preflight(request proto.LaunchRequest) error {
	if _, ok := runnerCommandPath(request.Info.Cmd, request.Info.Cwd, request.Env["PATH"]); !ok {
		return fmt.Errorf(
			"session command %q is not executable in the Sessions runner PATH; install it under ~/.local/bin, Homebrew, /usr/local/bin, or choose another agent",
			request.Info.Cmd,
		)
	}
	return nil
}

func (l *LaunchdLauncher) Launch(ctx context.Context, request proto.LaunchRequest) (proto.Runner, error) {
	if err := l.Preflight(request); err != nil {
		return nil, err
	}
	plist := plistPath(l.config.LaunchAgentsDir, request.Info.ID)
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	command := exec.Command("launchctl", "bootstrap", domain, plist)
	output, err := command.CombinedOutput()
	if err != nil {
		var exitError *exec.ExitError
		alreadyLoaded := errors.As(err, &exitError) && exitError.ExitCode() == 17
		alreadyLoaded = alreadyLoaded || strings.Contains(strings.ToLower(string(output)), "already loaded") ||
			strings.Contains(strings.ToLower(string(output)), "already bootstrapped")
		if !alreadyLoaded {
			return nil, fmt.Errorf("launchctl bootstrap %s: %w: %s", request.Info.ID, err, strings.TrimSpace(string(output)))
		}
	}
	return l.waitAndAttach(ctx, request.Info)
}

func runnerCommandPath(command, cwd, pathValue string) (string, bool) {
	if strings.ContainsRune(command, filepath.Separator) {
		candidate := command
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(cwd, candidate)
		}
		for _, executable := range executableCandidates(candidate) {
			if isExecutableFile(executable) {
				return executable, true
			}
		}
		return candidate, false
	}
	for _, directory := range filepath.SplitList(pathValue) {
		if directory == "" {
			continue
		}
		for _, candidate := range executableCandidates(filepath.Join(directory, command)) {
			if isExecutableFile(candidate) {
				return candidate, true
			}
		}
	}
	return "", false
}

func (l *LaunchdLauncher) Attach(ctx context.Context, info proto.RunnerInfo) (proto.Runner, error) {
	if info.SocketPath == "" {
		info.SocketPath = For(l.config.RunnerStateDir, info.ID).Socket
	}
	return proto.DialRunner(ctx, info.SocketPath)
}

func (l *LaunchdLauncher) waitAndAttach(ctx context.Context, info proto.RunnerInfo) (proto.Runner, error) {
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for {
		runner, err := l.Attach(ctx, info)
		if err == nil {
			return runner, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("runner did not create socket within 60s: %s: %w", info.SocketPath, lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(30 * time.Millisecond):
		}
	}
}

// Reap unloads a cleanly exited runner so launchd cannot retain a stale
// service registration after its plist is removed.
func (l *LaunchdLauncher) Reap(id string) error {
	candidates := []struct {
		label string
		path  string
	}{
		{label: launchdLabelPrefix + id, path: plistPath(l.config.LaunchAgentsDir, id)},
		{label: legacyLaunchdLabelPrefix + id, path: LegacyRunnerPlistPath(l.config.LaunchAgentsDir, id)},
	}
	found := false
	var reapErrors []error
	for _, candidate := range candidates {
		if _, statErr := os.Stat(candidate.path); statErr != nil {
			if !errors.Is(statErr, os.ErrNotExist) {
				reapErrors = append(reapErrors, statErr)
			}
			continue
		}
		found = true
		domain := fmt.Sprintf("gui/%d/%s", os.Getuid(), candidate.label)
		output, bootoutErr := exec.Command("launchctl", "bootout", domain).CombinedOutput()
		if bootoutErr != nil {
			reapErrors = append(reapErrors,
				fmt.Errorf("launchctl bootout %s: %w: %s", candidate.label, bootoutErr, strings.TrimSpace(string(output))))
		}
		if removeErr := os.Remove(candidate.path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			reapErrors = append(reapErrors, removeErr)
		}
	}
	if !found {
		return nil
	}
	return errors.Join(reapErrors...)
}
