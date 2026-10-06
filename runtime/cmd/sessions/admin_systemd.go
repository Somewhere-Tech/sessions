package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

func (a *app) linuxServiceConfig(m *linuxServiceManager) (linuxServiceConfig, error) {
	name := os.Getenv("SESSIONS_DAEMON_LABEL")
	if name == "" {
		name = "sessions"
	}
	name, err := resolveDaemonLabel(name)
	if err != nil {
		return linuxServiceConfig{}, err
	}
	cli, err := m.executable()
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

// linuxServiceManager is every outside effect the Linux installer has. The
// command uses systemctl --user and the daemon's health endpoint; tests
// substitute a fake so no test ever restarts a real service.
type linuxServiceManager struct {
	lookPath   func(string) (string, error)
	systemctl  func(arguments ...string) ([]byte, error)
	healthy    func(name string) error
	linger     func() ([]byte, error)
	executable func() (string, error)
	writeUnit  func(path, content string) error
	chmodUnit  func(path string, mode fs.FileMode) error
}

func (a *app) linuxServices() *linuxServiceManager {
	if a.linuxService != nil {
		return a.linuxService
	}
	return &linuxServiceManager{lookPath: exec.LookPath, systemctl: userSystemctl,
		healthy: a.waitLinuxServiceHealthy, linger: loginctlLinger, executable: os.Executable,
		writeUnit: writeDaemonPlist, chmodUnit: os.Chmod}
}

func requireUserSystemd(m *linuxServiceManager) error {
	if _, err := m.lookPath("systemctl"); err != nil {
		return fail(2, "Linux service install requires systemd and systemctl; run sessionsd directly on hosts without systemd")
	}
	output, err := m.systemctl("show-environment")
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
	m := a.linuxServices()
	if err := requireUserSystemd(m); err != nil {
		return err
	}
	config, err := a.linuxServiceConfig(m)
	if err != nil {
		return err
	}
	baseline, err := readLinuxUnitBaseline(m, config)
	if err != nil {
		return err
	}
	if !baseline.active {
		if err := a.waitForDaemonPortAvailable(time.Second); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(config.Path), 0o700); err != nil {
		return err
	}
	if err := m.writeUnit(config.Path, linuxServiceUnit(config)); err != nil {
		return a.reportLinuxUnitWriteFailure(m, config, baseline, err)
	}
	activation, stepErr := applyLinuxUnit(m, config, baseline, restart)
	if stepErr != nil {
		return a.reportLinuxInstallFailure(config, stepErr, recoverLinuxInstall(m, config, baseline, stepErr))
	}
	return a.reportLinuxInstallSuccess(m, config, baseline.active && !restart, activation)
}

// linuxActivation is what the forward install did and observed. Health is
// null when no daemon was started or restarted; a healthy endpoint still does
// not identify the runtime binary that answered.
type linuxActivation struct {
	action             string
	killMode           string
	healthy            *bool
	enablement         string
	priorUnitFileState string
}

// applyLinuxUnit loads and enables a unit file that is already written. A
// first start stops nothing, so it needs no policy. A restart stops the
// running daemon under the loaded unit's effective KillMode -- which a drop-in
// or an administrator can change -- so it happens only when systemd reports
// process; otherwise the running daemon is left as it was.
func applyLinuxUnit(m *linuxServiceManager, config linuxServiceConfig, baseline linuxUnitBaseline, restart bool) (linuxActivation, *linuxInstallStepError) {
	steps := [][]string{{"daemon-reload"}}
	enablement := linuxEnablementAction(baseline.unitFileState)
	if enablement == linuxEnablementEnable {
		steps = append(steps, []string{"enable", config.Name})
	}
	for _, arguments := range steps {
		if output, err := m.systemctl(arguments...); err != nil {
			return linuxActivation{}, &linuxInstallStepError{step: arguments[0], detail: outputOrError(output, err)}
		}
	}
	mode, modeErr := effectiveKillMode(m, config.Name)
	activation := linuxActivation{action: "preserved", killMode: mode, enablement: enablement, priorUnitFileState: baseline.unitFileState}
	if baseline.active && !restart {
		return activation, nil
	}
	activation.action = "start"
	if baseline.active {
		activation.action = "restart"
		if modeErr != nil || mode != "process" {
			reason := "systemd reports its effective KillMode as " + mode
			if modeErr != nil {
				reason = "its effective KillMode could not be read (" + modeErr.Error() + ")"
			}
			return activation, &linuxInstallStepError{step: "kill_mode", detail: "the running daemon was not restarted because " + reason +
				", and a restart under any policy but process could signal runners; review systemctl --user cat " + config.Name + " and its drop-ins"}
		}
	}
	if output, err := m.systemctl(activation.action, config.Name); err != nil {
		return activation, &linuxInstallStepError{step: activation.action, detail: outputOrError(output, err), daemonTouched: true}
	}
	if err := m.healthy(config.Name); err != nil {
		return activation, &linuxInstallStepError{step: "health", detail: err.Error(), daemonTouched: true}
	}
	healthy := true
	activation.healthy = &healthy
	return activation, nil
}

// reportLinuxInstallSuccess states what was done and observed. It makes no
// promise about runner lifetime: that depends on the effective KillMode it
// reports and on policy outside Sessions.
func (a *app) reportLinuxInstallSuccess(m *linuxServiceManager, config linuxServiceConfig, restartRequired bool, activation linuxActivation) error {
	linger := userLingerFrom(m.linger())
	state := observedServiceState(m, config.Name)
	policy := "Effective KillMode: " + activation.killMode + "."
	if activation.killMode == "process" {
		policy += " Under it a systemd stop or restart signals only the daemon process."
	} else {
		policy += " A later stop or restart under this policy could signal runners; review systemctl --user cat " + config.Name + " and its drop-ins."
	}
	health := "not checked (no daemon was started or restarted)"
	if activation.healthy != nil {
		health = "answered its health check"
	}
	result := map[string]any{"ok": true, "service": config.Name, "unit_path": config.Path, "runtime_path": filepath.Dir(config.Daemon),
		"configuration": "installed", "loaded": true, "daemon_action": activation.action, "kill_mode": activation.killMode,
		"service_state": state, "healthy": activation.healthy, "runtime_identity": "not_verified", "restart_required": restartRequired,
		"enablement": activation.enablement, "unit_file_state": observedUnitFileState(m, config.Name),
		"boot_requirement": "loginctl enable-linger USER (explicit user/admin action)", "linger": linger.State, "linger_note": linger.note()}
	if a.wantJSON {
		return writeJSON(a.stdout, result, true)
	}
	fmt.Fprintf(a.stdout, "Installed and loaded %s; daemon action: %s; service state: %s; health: %s; runtime identity not verified.\nUnit: %s\n%s\n",
		config.Name, activation.action, state, health, config.Path, policy)
	if restartRequired {
		fmt.Fprintln(a.stdout, "The running daemon keeps its current version. Apply the staged version with sessions install --restart-daemon.")
	}
	if note := linuxEnablementNote(activation.enablement, activation.priorUnitFileState, config.Name); note != "" {
		fmt.Fprintln(a.stdout, note)
	}
	fmt.Fprintln(a.stdout, linger.note()+"\nLogs: journalctl --user -u "+config.Name)
	return nil
}

// userLinger is the Linger property logind reports for this user: an
// observed setting, not a forecast. Under standard systemd defaults a user
// without lingering loses user@UID.service shortly after the last login
// session ends (UserStopDelaySec=10s), and that unit's KillMode=mixed kills
// every process left in its control group -- including each runner this
// service's daemon started. A host can change that (UserStopDelaySec=infinity,
// distribution or administrator policy), and lingering does not protect work
// from crashes or shutdown, so Sessions reports the setting and the standard
// behaviour, never a guarantee. Enabling lingering is the user's decision.
type userLinger struct {
	State  string // enabled, disabled, or unknown
	Detail string
}

func loginctlLinger() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "loginctl", "show-user", strconv.Itoa(os.Getuid()), "--property=Linger", "--value").CombinedOutput()
}

func userLingerFrom(output []byte, err error) userLinger {
	if err != nil {
		return userLinger{State: "unknown", Detail: outputOrError(output, err)}
	}
	switch value := strings.TrimSpace(string(output)); value {
	case "yes":
		return userLinger{State: "enabled"}
	case "no":
		return userLinger{State: "disabled"}
	default:
		return userLinger{State: "unknown", Detail: fmt.Sprintf("loginctl reported Linger=%q", value)}
	}
}

const lingerPolicyCaveat = "This host's logind policy was not inspected and can differ."

func (l userLinger) note() string {
	switch l.State {
	case "enabled":
		return "Lingering is enabled (loginctl Linger=yes): systemd keeps your user manager, and the services it runs, after you log out and starts it at boot. It does not protect work from crashes, shutdown, or other host policy."
	case "disabled":
		return "Lingering is off (loginctl Linger=no). Under standard systemd defaults your user manager stops about 10 seconds after your last login session, including SSH, ends, and that ends every runner this service started. " + lingerPolicyCaveat + " To keep work running after logout, run: loginctl enable-linger \"$USER\" (may need administrator authorization)."
	}
	return "Could not read this user's lingering state (" + l.Detail + "). Under standard systemd defaults, without lingering, logging out ends every runner this service started. " + lingerPolicyCaveat + " Check with: loginctl show-user \"$USER\" --property=Linger; enable with: loginctl enable-linger \"$USER\"."
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
	m := a.linuxServices()
	if err := requireUserSystemd(m); err != nil {
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
	if output, err := m.systemctl("disable", name); err != nil {
		return fail(2, "disable %s: %s", name, outputOrError(output, err))
	}
	path := filepath.Join(a.home, ".config", "systemd", "user", name)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if output, err := m.systemctl("daemon-reload"); err != nil {
		return fail(2, "reload user services: %s", outputOrError(output, err))
	}
	if a.wantJSON {
		return writeJSON(a.stdout, map[string]any{"ok": true, "service": name, "removed": path, "daemon_stopped": false, "runners_preserved": true, "state_preserved": true}, true)
	}
	fmt.Fprintln(a.stdout, "Removed login integration for "+name+". The daemon, runners, runtime bytes, and history were preserved.")
	return nil
}
