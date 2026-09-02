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
	_, err = writePlist(l.config.LaunchAgentsDir, plistArgs{
		ID:               request.Info.ID,
		ProgramArguments: l.ProgramArguments(request),
		Env:              request.Env,
		Cwd:              request.Info.Cwd,
		LogPath:          filepath.Join(l.config.RunnerStateDir, request.Info.ID+".log"),
		KeepAlivePath:    paths.KeepAlive,
	})
	if err != nil {
		_ = os.Remove(paths.KeepAlive)
	}
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

// Wake restarts a runner that stayed paused after a reboot: renew its
// same-boot permit so the runner accepts the launch, kick the launchd job,
// and attach. A runner that still refuses gets its paused marker back, so
// the session never reads as unknown.
func (l *LaunchdLauncher) Wake(ctx context.Context, id string) (proto.Runner, error) {
	paths := For(l.config.RunnerStateDir, id)
	pending, pendingErr := ReadRestorePending(paths.RestorePending)
	if pendingErr != nil {
		return nil, fmt.Errorf("session %s is not paused after a reboot: %w", id, pendingErr)
	}
	bootID, err := CurrentBootID()
	if err != nil {
		return nil, err
	}
	if err := WriteRestartPermit(paths.KeepAlive, bootID); err != nil {
		return nil, fmt.Errorf("renew runner permit: %w", err)
	}
	_ = os.Remove(paths.RestorePending)
	restorePaused := func(reason string) {
		_ = os.Remove(paths.KeepAlive)
		_ = WriteRestorePending(paths.RestorePending, id, pending.Reason+" (last wake attempt: "+reason+")")
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	plist := plistPath(l.config.LaunchAgentsDir, id)
	label := launchdLabelPrefix + id
	if _, statErr := os.Stat(plist); statErr != nil {
		plist = LegacyRunnerPlistPath(l.config.LaunchAgentsDir, id)
		label = legacyLaunchdLabelPrefix + id
	}
	// The job is usually still loaded from boot, in which case bootstrap
	// would refuse; load it only when launchd does not know it.
	if exec.Command("launchctl", "print", domain+"/"+label).Run() != nil {
		if output, bootErr := exec.Command("launchctl", "bootstrap", domain, plist).CombinedOutput(); bootErr != nil {
			restorePaused("launchctl bootstrap: " + strings.TrimSpace(string(output)))
			return nil, fmt.Errorf("launchctl bootstrap %s: %w: %s", id, bootErr, strings.TrimSpace(string(output)))
		}
	}
	if output, kickErr := exec.Command("launchctl", "kickstart", domain+"/"+label).CombinedOutput(); kickErr != nil {
		restorePaused("launchctl kickstart: " + strings.TrimSpace(string(output)))
		return nil, fmt.Errorf("launchctl kickstart %s: %w: %s", id, kickErr, strings.TrimSpace(string(output)))
	}
	runner, err := l.waitAndAttach(ctx, proto.RunnerInfo{ID: id, SocketPath: paths.Socket})
	if err != nil {
		restorePaused(err.Error())
		return nil, err
	}
	return runner, nil
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
	for _, path := range []string{
		For(l.config.RunnerStateDir, id).KeepAlive,
		For(l.config.RunnerStateDir, id).RestorePending,
	} {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			reapErrors = append(reapErrors, removeErr)
		}
	}
	if !found && len(reapErrors) == 0 {
		return nil
	}
	return errors.Join(reapErrors...)
}
