package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type plistEnvironment struct {
	Key   string
	Value string
}

type daemonPlistOptions struct {
	Label            string
	ProgramArguments []string
	WorkingDir       string
	LogFile          string
	Env              []plistEnvironment
}

func daemonPlist(options daemonPlistOptions) string {
	var programArguments strings.Builder
	for _, argument := range options.ProgramArguments {
		fmt.Fprintf(&programArguments, "    <string>%s</string>\n", escapeXML(argument))
	}
	var environment strings.Builder
	for _, entry := range options.Env {
		fmt.Fprintf(&environment, "    <key>%s</key>\n    <string>%s</string>\n", escapeXML(entry.Key), escapeXML(entry.Value))
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
%s
  </array>
  <key>EnvironmentVariables</key>
  <dict>
%s  </dict>
  <key>WorkingDirectory</key>
  <string>%s</string>
  <key>RunAtLoad</key>
  <true/>
  <!-- KeepAlive=true: the daemon itself should restart on crash. This is
       distinct from the per-session runner plists, which use KeepAlive only
       on non-zero exit (SuccessfulExit=false). -->
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, escapeXML(options.Label), strings.TrimSuffix(programArguments.String(), "\n"), environment.String(),
		escapeXML(options.WorkingDir), escapeXML(options.LogFile), escapeXML(options.LogFile))
}

func escapeXML(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	return strings.ReplaceAll(value, ">", "&gt;")
}

const defaultDaemonLabel = "tech.somewhere.sessions.dev.daemon"

var (
	validDaemonLabel     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
	regexpServiceMissing = regexp.MustCompile(`(?i)(could not find specified service|no such process|service not found)`)
)

type daemonInstallConfig struct {
	Label      string
	DaemonPath string
	RunnerPath string
	PlistPath  string
	LogFile    string
	Env        []plistEnvironment
}

func resolveDaemonLabel(value string) (string, error) {
	if value == "" {
		value = defaultDaemonLabel
	}
	if len(value) > 128 || !validDaemonLabel.MatchString(value) {
		return "", fail(2, "invalid SESSIONS_DAEMON_LABEL %q: use letters, digits, dots, or hyphens", value)
	}
	return value, nil
}

func locateInstallBinary(name, explicit string, searchDirs ...string) string {
	if explicit != "" {
		resolved, err := filepath.Abs(explicit)
		if err == nil && executableFile(resolved) {
			return resolved
		}
		return ""
	}
	names := []string{name, fmt.Sprintf("%s-%s-%s", name, runtime.GOOS, runtime.GOARCH)}
	seen := make(map[string]bool)
	for _, directory := range searchDirs {
		if directory == "" || seen[directory] {
			continue
		}
		seen[directory] = true
		for _, candidateName := range names {
			candidate := filepath.Join(directory, candidateName)
			if executableFile(candidate) {
				resolved, err := filepath.Abs(candidate)
				if err == nil {
					return resolved
				}
			}
		}
	}
	if candidate, err := exec.LookPath(name); err == nil {
		if resolved, absErr := filepath.Abs(candidate); absErr == nil && executableFile(resolved) {
			return resolved
		}
	}
	return ""
}

func launchctlExecutable() string {
	for _, candidate := range []string{"/bin/launchctl", "/usr/bin/launchctl"} {
		if executableFile(candidate) {
			return candidate
		}
	}
	return ""
}

func (a *app) daemonInstallConfig() (daemonInstallConfig, error) {
	label, err := resolveDaemonLabel(os.Getenv("SESSIONS_DAEMON_LABEL"))
	if err != nil {
		return daemonInstallConfig{}, err
	}
	executable, _ := os.Executable()
	cliDir := filepath.Dir(executable)
	daemonPath := locateInstallBinary("sessionsd", os.Getenv("SESSIONS_BINARY"), cliDir)
	if daemonPath == "" {
		return daemonInstallConfig{}, fail(2, "install is incomplete: missing Go daemon binary 'sessionsd' beside sessions or on PATH; install all three Sessions binaries together")
	}
	runnerPath := locateInstallBinary("sessions-runner", os.Getenv("SESSIONS_RUNNER"), filepath.Dir(daemonPath), cliDir)
	if runnerPath == "" {
		return daemonInstallConfig{}, fail(2, "install is incomplete: missing Go runner binary 'sessions-runner' beside sessionsd or sessions or on PATH; install all three Sessions binaries together")
	}
	logDir := filepath.Join(a.home, "Library", "Logs", "sessions")
	agentsDir := filepath.Join(a.home, "Library", "LaunchAgents")
	const daemonPATH = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
	environment := []plistEnvironment{
		{Key: "PATH", Value: daemonPATH},
		{Key: "SESSIONS_HOST", Value: a.host},
		{Key: "SESSIONS_PORT", Value: a.port},
		{Key: "SESSIONS_RUNNER", Value: runnerPath},
	}
	for _, key := range []string{"SESSIONS_WEB_DIR", "SESSIONS_STATE_DIR", "SESSIONS_LEDGER_PATH"} {
		if value := os.Getenv(key); value != "" {
			environment = append(environment, plistEnvironment{Key: key, Value: value})
		}
	}
	return daemonInstallConfig{
		Label: label, DaemonPath: daemonPath, RunnerPath: runnerPath,
		PlistPath: filepath.Join(agentsDir, label+".plist"),
		LogFile:   filepath.Join(logDir, label+".log"),
		Env:       environment,
	}, nil
}

func writeDaemonPlist(path, xml string) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".sessions-daemon-plist-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := io.WriteString(temporary, xml); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func launchctlServiceMissing(output []byte, err error) bool {
	return err != nil && regexpServiceMissing.Match(output)
}

func (a *app) waitForDaemonPortAvailable(timeout time.Duration) error {
	address := net.JoinHostPort(a.host, a.port)
	deadline := a.now().Add(timeout)
	for {
		connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err != nil {
			return nil
		}
		_ = connection.Close()
		if !a.now().Before(deadline) {
			return fail(2, "cannot install development daemon: %s is already accepting connections from another process", address)
		}
		a.sleep(100 * time.Millisecond)
	}
}

func (a *app) cmdInstall(args []string) error {
	if len(args) != 0 {
		return fail(1, "usage: sessions install")
	}
	if runtime.GOOS != "darwin" {
		return fail(2, "install requires macOS launchd")
	}
	config, err := a.daemonInstallConfig()
	if err != nil {
		return err
	}
	launchctl := launchctlExecutable()
	if launchctl == "" {
		return fail(2, "install requires launchctl, but it was not found in /bin or /usr/bin")
	}
	if err := os.MkdirAll(filepath.Dir(config.PlistPath), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(config.LogFile), 0o700); err != nil {
		return err
	}
	xml := daemonPlist(daemonPlistOptions{
		Label: config.Label, ProgramArguments: []string{config.DaemonPath},
		WorkingDir: filepath.Dir(config.DaemonPath), LogFile: config.LogFile, Env: config.Env,
	})
	if err := writeDaemonPlist(config.PlistPath, xml); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "wrote plist: %s\n", config.PlistPath)
	uid := os.Getuid()
	domain := fmt.Sprintf("gui/%d", uid)
	serviceTarget := domain + "/" + config.Label
	bootout := exec.Command(launchctl, "bootout", serviceTarget)
	bootoutOutput, bootoutErr := bootout.CombinedOutput()
	if bootoutErr != nil && !launchctlServiceMissing(bootoutOutput, bootoutErr) {
		return fail(2, "launchctl bootout before reinstall failed (status=%s): %s", commandStatus(bootoutErr), outputOrError(bootoutOutput, bootoutErr))
	}
	if err := a.waitForDaemonPortAvailable(2 * time.Second); err != nil {
		return err
	}
	command := exec.Command(launchctl, "bootstrap", domain, config.PlistPath)
	output, err := command.CombinedOutput()
	if err != nil {
		return fail(2, "launchctl bootstrap failed (status=%s): %s", commandStatus(err), outputOrError(output, err))
	}
	deadline := a.now().Add(15 * time.Second)
	lastHealthError := "no response"
	for a.now().Before(deadline) {
		response, requestErr := a.api.request(context.Background(), "GET", "/api/health", nil, time.Second)
		if requestErr == nil && response.status == 200 {
			lastHealthError = ""
			break
		}
		if requestErr != nil {
			lastHealthError = requestErr.Error()
		} else {
			lastHealthError = fmt.Sprintf("HTTP %d", response.status)
		}
		a.sleep(250 * time.Millisecond)
	}
	if lastHealthError != "" {
		return fail(2, "daemon did not become healthy at http://%s:%s/api/health within 15s (%s); see %s", a.host, a.port, lastHealthError, config.LogFile)
	}
	token, tokenErr := a.api.readToken()
	if tokenErr != nil {
		return fail(2, "%s", tokenErr)
	}
	io.WriteString(a.stdout, "\nsessionsd development daemon registered, started, and healthy.\n")
	fmt.Fprintf(a.stdout, "  Label: %s\n", config.Label)
	fmt.Fprintf(a.stdout, "  URL:   http://%s:%s\n", a.host, a.port)
	if token != "" {
		fmt.Fprintf(a.stdout, "  Token: %s\n", token)
		io.WriteString(a.stdout, "\nPaste the URL and token into the sessions web UI (server settings).\n")
	} else {
		io.WriteString(a.stdout, "\nToken not yet generated — give the daemon a moment, then run: sessions token\n")
	}
	fmt.Fprintf(a.stdout, "  Logs:  %s\n", config.LogFile)
	return nil
}

func (a *app) cmdUninstall(args []string) error {
	if len(args) != 0 {
		return fail(1, "usage: sessions uninstall")
	}
	if runtime.GOOS != "darwin" {
		return fail(2, "uninstall requires macOS launchd")
	}
	label, err := resolveDaemonLabel(os.Getenv("SESSIONS_DAEMON_LABEL"))
	if err != nil {
		return err
	}
	launchctl := launchctlExecutable()
	if launchctl == "" {
		return fail(2, "uninstall requires launchctl, but it was not found in /bin or /usr/bin")
	}
	serviceTarget := fmt.Sprintf("gui/%d/%s", os.Getuid(), label)
	command := exec.Command(launchctl, "bootout", serviceTarget)
	output, bootoutErr := command.CombinedOutput()
	if bootoutErr != nil && !launchctlServiceMissing(output, bootoutErr) {
		return fail(2, "launchctl bootout failed (status=%s): %s", commandStatus(bootoutErr), outputOrError(output, bootoutErr))
	}
	plistPath := filepath.Join(a.home, "Library", "LaunchAgents", label+".plist")
	removed := true
	if err := os.Remove(plistPath); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		removed = false
	}
	if bootoutErr != nil && !removed {
		fmt.Fprintf(a.stdout, "sessionsd development daemon already uninstalled (label %s)\n", label)
		return nil
	}
	fmt.Fprintf(a.stdout, "sessionsd development daemon uninstalled (label %s)\n", label)
	fmt.Fprintf(a.stdout, "state and logs were preserved; removed plist: %s\n", plistPath)
	return nil
}

func commandStatus(err error) string {
	if exitError, ok := err.(*exec.ExitError); ok {
		return strconv.Itoa(exitError.ExitCode())
	}
	return "unknown"
}

func outputOrError(output []byte, err error) string {
	if text := strings.TrimSpace(string(output)); text != "" {
		return text
	}
	if err != nil {
		return err.Error()
	}
	return "unknown error"
}

func (a *app) cmdDeploy(args []string) error {
	if len(args) != 0 {
		return fail(1, "usage: sessions deploy")
	}
	return fail(2, "sessions deploy was retired with the Node daemon; no changes were made. Sessions.app is the macOS install/update path. See docs/RELEASE.md and docs/NATIVE_APP.md in the source repository")
}
