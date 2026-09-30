package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type linuxServiceConfig struct {
	Name   string
	Path   string
	Daemon string
	Runner string
	Env    []plistEnvironment
}

// systemdQuote protects both the unit parser and specifier expansion. ExecStart
// is a direct executable invocation, never a shell command.
func systemdQuote(value string) string {
	value = strings.ReplaceAll(value, "%", "%%")
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	value = strings.ReplaceAll(value, "\n", "\\n")
	value = strings.ReplaceAll(value, "\r", "\\r")
	return "\"" + value + "\""
}

func linuxServiceUnit(config linuxServiceConfig) string {
	var environment strings.Builder
	for _, entry := range config.Env {
		fmt.Fprintf(&environment, "Environment=%s\n", systemdQuote(entry.Key+"="+entry.Value))
	}
	return fmt.Sprintf(`[Unit]
Description=Sessions durable conversation daemon
After=network.target

[Service]
Type=simple
ExecStart=%s
%sRestart=on-failure
RestartSec=2
# Runners own their work independently. A daemon stop must signal only the daemon.
KillMode=process
TimeoutStopSec=10
UMask=0077

[Install]
WantedBy=default.target
`, systemdQuote(strings.ReplaceAll(config.Daemon, "$", "$$")), environment.String())
}

func (a *app) linuxServiceConfig() (linuxServiceConfig, error) {
	name := os.Getenv("SESSIONS_DAEMON_LABEL")
	if name == "" {
		name = "sessions"
	}
	name, err := resolveDaemonLabel(name)
	if err != nil {
		return linuxServiceConfig{}, err
	}
	cli, err := os.Executable()
	if err != nil {
		return linuxServiceConfig{}, err
	}
	daemon := locateInstallBinary("sessionsd", os.Getenv("SESSIONS_BINARY"), filepath.Dir(cli))
	runner := locateInstallBinary("sessions-runner", os.Getenv("SESSIONS_RUNNER"), filepath.Dir(daemon), filepath.Dir(cli))
	if daemon == "" || runner == "" {
		return linuxServiceConfig{}, fail(2, "Linux install needs sessionsd and sessions-runner beside sessions or on PATH; reinstall all three binaries together")
	}
	staged, err := stageLinuxRuntime(a.home, map[string]string{"sessions": cli, "sessionsd": daemon, "sessions-runner": runner})
	if err != nil {
		return linuxServiceConfig{}, err
	}
	environment := []plistEnvironment{
		{Key: "HOME", Value: a.home},
		{Key: "PATH", Value: os.Getenv("PATH")},
		{Key: "SESSIONS_HOST", Value: a.host}, {Key: "SESSIONS_PORT", Value: a.port},
		{Key: "SESSIONS_RUNNER", Value: filepath.Join(staged, "sessions-runner")},
	}
	for _, key := range []string{"SESSIONS_STATE_DIR", "SESSIONS_LEDGER_PATH", "SESSIONS_WEB_DIR", "SESSIONS_PPROF", "SHELL"} {
		if value := os.Getenv(key); value != "" {
			environment = append(environment, plistEnvironment{Key: key, Value: value})
		}
	}
	return linuxServiceConfig{Name: name + ".service", Path: filepath.Join(a.home, ".config", "systemd", "user", name+".service"),
		Daemon: filepath.Join(staged, "sessionsd"), Runner: filepath.Join(staged, "sessions-runner"), Env: environment}, nil
}

// Stage a content-addressed snapshot outside the package installation. npm
// upgrades may remove their old directory while old runners still execute it.
func stageLinuxRuntime(home string, sources map[string]string) (string, error) {
	root := filepath.Join(home, ".local", "share", "sessions", "runtime")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	temporary, err := os.MkdirTemp(root, ".stage-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temporary)
	for _, name := range []string{"sessions", "sessionsd", "sessions-runner"} {
		if err := copyRuntimeBinary(sources[name], filepath.Join(temporary, name)); err != nil {
			return "", err
		}
	}
	digest, err := runtimeDirectoryDigest(temporary)
	if err != nil {
		return "", err
	}
	final := filepath.Join(root, digest)
	if _, err := os.Stat(final); err == nil {
		cachedDigest, err := runtimeDirectoryDigest(final)
		if err != nil || cachedDigest != digest {
			return "", fail(2, "immutable runtime cache is incomplete or changed at %s; preserve running processes and reinstall to a new runtime directory", final)
		}
		return final, nil
	}
	if err := os.Rename(temporary, final); err != nil {
		return "", err
	}
	return final, nil
}

func runtimeDirectoryDigest(dir string) (string, error) {
	digest := sha256.New()
	for _, name := range []string{"sessions", "sessionsd", "sessions-runner"} {
		file, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return "", err
		}
		_, _ = io.WriteString(digest, name+"\x00")
		_, copyErr := io.Copy(digest, file)
		_ = file.Close()
		if copyErr != nil {
			return "", copyErr
		}
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}

func copyRuntimeBinary(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func userSystemctl(arguments ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, arguments...)...).CombinedOutput()
}

func requireUserSystemd() error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fail(2, "Linux service install requires systemd and systemctl; run sessionsd directly on hosts without systemd")
	}
	output, err := userSystemctl("show-environment")
	if err != nil {
		return fail(2, "cannot reach your systemd user manager: %s; log in as the intended non-root user; for boot operation enable lingering with `loginctl enable-linger USER`", outputOrError(output, err))
	}
	return nil
}

func (a *app) installLinuxService(args []string) error {
	restart := false
	if len(args) == 1 && args[0] == "--restart-daemon" {
		restart = true
	} else if len(args) != 0 {
		return fail(1, "usage: sessions install [--restart-daemon]")
	}
	if err := requireUserSystemd(); err != nil {
		return err
	}
	config, err := a.linuxServiceConfig()
	if err != nil {
		return err
	}
	_, activeErr := userSystemctl("is-active", "--quiet", config.Name)
	if activeErr != nil {
		if err := a.waitForDaemonPortAvailable(time.Second); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(config.Path), 0o700); err != nil {
		return err
	}
	if err := writeDaemonPlist(config.Path, linuxServiceUnit(config)); err != nil {
		return err
	}
	for _, arguments := range [][]string{{"daemon-reload"}, {"enable", config.Name}} {
		if output, err := userSystemctl(arguments...); err != nil {
			return fail(2, "systemctl %s failed: %s", strings.Join(arguments, " "), outputOrError(output, err))
		}
	}
	action := "preserved"
	if activeErr != nil || restart {
		verb := "start"
		if restart && activeErr == nil {
			verb = "restart"
		}
		if output, err := userSystemctl(verb, config.Name); err != nil {
			return fail(2, "systemctl %s failed: %s; inspect `journalctl --user -u %s`", verb, outputOrError(output, err), config.Name)
		}
		if err := a.waitLinuxServiceHealthy(config.Name); err != nil {
			return err
		}
		action = verb
	}
	result := map[string]any{"ok": true, "service": config.Name, "unit_path": config.Path, "runtime_path": filepath.Dir(config.Daemon), "daemon_action": action,
		"restart_required": activeErr == nil && !restart, "boot_requirement": "loginctl enable-linger USER (explicit user/admin action)", "runners_preserved": true}
	if a.wantJSON {
		return writeJSON(a.stdout, result, true)
	}
	fmt.Fprintf(a.stdout, "Installed %s; daemon action: %s. Runners were not stopped.\nUnit: %s\n", config.Name, action, config.Path)
	if activeErr == nil && !restart {
		fmt.Fprintln(a.stdout, "The running daemon keeps its current version. Apply the staged version with sessions install --restart-daemon.")
	}
	fmt.Fprintln(a.stdout, "For operation before login and after logout, explicitly enable lingering: loginctl enable-linger USER\nLogs: journalctl --user -u "+config.Name)
	return nil
}

func (a *app) waitLinuxServiceHealthy(name string) error {
	deadline := a.now().Add(15 * time.Second)
	for a.now().Before(deadline) {
		response, err := a.api.request(context.Background(), "GET", "/api/health", nil, time.Second)
		if err == nil && response.status == 200 {
			return nil
		}
		a.sleep(250 * time.Millisecond)
	}
	return fail(2, "daemon did not become healthy within 15 seconds; inspect `journalctl --user -u %s`", name)
}

// Removing login integration never stops the daemon or any runner.
func (a *app) uninstallLinuxService(args []string) error {
	if len(args) != 0 {
		return fail(1, "usage: sessions uninstall")
	}
	if err := requireUserSystemd(); err != nil {
		return err
	}
	name := os.Getenv("SESSIONS_DAEMON_LABEL")
	if name == "" {
		name = "sessions"
	}
	name, err := resolveDaemonLabel(name)
	if err != nil {
		return err
	}
	name += ".service"
	if output, err := userSystemctl("disable", name); err != nil {
		return fail(2, "disable %s: %s", name, outputOrError(output, err))
	}
	path := filepath.Join(a.home, ".config", "systemd", "user", name)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if output, err := userSystemctl("daemon-reload"); err != nil {
		return fail(2, "reload user services: %s", outputOrError(output, err))
	}
	if a.wantJSON {
		return writeJSON(a.stdout, map[string]any{"ok": true, "service": name, "removed": path, "daemon_stopped": false, "runners_preserved": true, "state_preserved": true}, true)
	}
	fmt.Fprintln(a.stdout, "Removed login integration for "+name+". The daemon, runners, runtime bytes, and history were preserved.")
	return nil
}
