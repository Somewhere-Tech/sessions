//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const windowsScratchDaemonToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestWindowsScratchDaemonTokenContract(t *testing.T) {
	moduleRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binaryRoot := t.TempDir()
	sessionsBinary := filepath.Join(binaryRoot, "sessions.exe")
	daemonBinary := filepath.Join(binaryRoot, "sessionsd.exe")
	buildWindowsTokenContractBinary(t, moduleRoot, sessionsBinary, "./cmd/sessions")
	buildWindowsTokenContractBinary(t, moduleRoot, daemonBinary, "./cmd/sessionsd")

	scratchRoot := t.TempDir()
	stateRoot := filepath.Join(scratchRoot, "state")
	tokenPath := filepath.Join(stateRoot, "token")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(windowsScratchDaemonToken), 0o600); err != nil {
		t.Fatal(err)
	}
	port := reserveWindowsTokenContractPort(t)
	environment := windowsTokenContractEnvironment(scratchRoot, stateRoot, port)
	baseURL := "http://127.0.0.1:" + port

	daemon := startWindowsTokenContractDaemon(t, daemonBinary, environment)
	waitForWindowsTokenContractHealth(t, daemon, baseURL)

	protected, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(protected, []byte(windowsScratchDaemonToken)) {
		t.Fatal("real daemon start left the legacy master token in plaintext")
	}
	stdout, stderr, exitCode := runWindowsTokenContractCLI(
		t,
		sessionsBinary,
		environment,
		"token",
	)
	if exitCode != 0 || stderr != "" || stdout != windowsScratchDaemonToken+"\n" {
		t.Fatalf("sessions token after daemon migration: exit=%d stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	assertWindowsTokenContractLocalCLI(t, sessionsBinary, environment)
	status, body := windowsTokenContractForwardedRequest(t, baseURL, windowsScratchDaemonToken)
	if status != http.StatusOK {
		t.Fatalf("master-token request after migration: status=%d body=%s", status, body)
	}
	daemon.stop(t)

	const damaged = `{"version":1,"protected":"not-ciphertext"}`
	if err := os.WriteFile(tokenPath, []byte(damaged), 0o600); err != nil {
		t.Fatal(err)
	}
	daemon = startWindowsTokenContractDaemon(t, daemonBinary, environment)
	waitForWindowsTokenContractHealth(t, daemon, baseURL)
	assertWindowsTokenContractLocalCLI(t, sessionsBinary, environment)

	stdout, stderr, exitCode = runWindowsTokenContractCLI(
		t,
		sessionsBinary,
		environment,
		"token",
	)
	if exitCode != 2 || stdout != "" ||
		!strings.Contains(stderr, "signed-in Windows user") ||
		!strings.Contains(stderr, "only if you intend to rotate master-token access") {
		t.Fatalf("sessions token with corrupt protected state: exit=%d stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	status, body = windowsTokenContractForwardedRequest(t, baseURL, windowsScratchDaemonToken)
	if status != http.StatusInternalServerError ||
		!strings.Contains(body, "only if you intend to rotate master-token access") {
		t.Fatalf("master-token request with corrupt protected state: status=%d body=%s", status, body)
	}
	after, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != damaged {
		t.Fatalf("corrupt protected state was rewritten or rotated: %q", after)
	}
	daemon.stop(t)
}

func buildWindowsTokenContractBinary(t *testing.T, moduleRoot, output, packagePath string) {
	t.Helper()
	command := exec.Command("go", "build", "-o", output, packagePath)
	command.Dir = moduleRoot
	command.Env = append(os.Environ(), "GOFLAGS=-buildvcs=false")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", packagePath, err, output)
	}
}

func reserveWindowsTokenContractPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return strconv.Itoa(port)
}

func windowsTokenContractEnvironment(scratchRoot, stateRoot, port string) []string {
	environment := make([]string, 0, len(os.Environ())+8)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "SESSIONS_") ||
			strings.HasPrefix(upper, "RUNNER_") ||
			upper == "HOME" ||
			upper == "USERPROFILE" ||
			upper == "LOCALAPPDATA" {
			continue
		}
		environment = append(environment, entry)
	}
	return append(
		environment,
		"HOME="+scratchRoot,
		"USERPROFILE="+scratchRoot,
		"LOCALAPPDATA="+filepath.Join(scratchRoot, "local-app-data"),
		"SESSIONS_HOST=127.0.0.1",
		"SESSIONS_PORT="+port,
		"SESSIONS_STATE_DIR="+stateRoot,
		"SESSIONS_LEDGER_PATH="+filepath.Join(scratchRoot, "ledger", "lanes.sqlite3"),
		"SESSIONS_WEB_DIR="+filepath.Join(scratchRoot, "web"),
	)
}

type windowsTokenContractDaemon struct {
	command *exec.Cmd
	logs    bytes.Buffer
	stopped bool
}

func startWindowsTokenContractDaemon(
	t *testing.T,
	binary string,
	environment []string,
) *windowsTokenContractDaemon {
	t.Helper()
	daemon := &windowsTokenContractDaemon{}
	daemon.command = exec.Command(binary, "--serve")
	daemon.command.Env = environment
	daemon.command.Stdout = &daemon.logs
	daemon.command.Stderr = &daemon.logs
	if err := daemon.command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		daemon.stop(t)
	})
	return daemon
}

func (d *windowsTokenContractDaemon) stop(t *testing.T) {
	t.Helper()
	if d == nil || d.stopped {
		return
	}
	d.stopped = true
	if d.command.Process != nil {
		_ = d.command.Process.Kill()
	}
	waited := make(chan error, 1)
	go func() {
		waited <- d.command.Wait()
	}()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatalf("scratch daemon did not exit; logs:\n%s", d.logs.String())
	}
}

func waitForWindowsTokenContractHealth(
	t *testing.T,
	daemon *windowsTokenContractDaemon,
	baseURL string,
) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(baseURL + "/api/health")
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr == nil && response.StatusCode == http.StatusOK &&
				bytes.Contains(body, []byte(`"name":"sessionsd"`)) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	daemon.stop(t)
	t.Fatalf("scratch daemon did not become healthy; logs:\n%s", daemon.logs.String())
}

func runWindowsTokenContractCLI(
	t *testing.T,
	binary string,
	environment []string,
	arguments ...string,
) (stdout string, stderr string, exitCode int) {
	t.Helper()
	command := exec.Command(binary, arguments...)
	command.Env = environment
	var stdoutBuffer, stderrBuffer bytes.Buffer
	command.Stdout = &stdoutBuffer
	command.Stderr = &stderrBuffer
	err := command.Run()
	if err == nil {
		return stdoutBuffer.String(), stderrBuffer.String(), 0
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("run sessions %s: %v", strings.Join(arguments, " "), err)
	}
	return stdoutBuffer.String(), stderrBuffer.String(), exitError.ExitCode()
}

func assertWindowsTokenContractLocalCLI(
	t *testing.T,
	binary string,
	environment []string,
) {
	t.Helper()
	stdout, stderr, exitCode := runWindowsTokenContractCLI(
		t,
		binary,
		environment,
		"--json",
		"ls",
	)
	if exitCode != 0 || stderr != "" {
		t.Fatalf("loopback sessions --json ls: exit=%d stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	var output any
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("loopback sessions --json ls returned invalid JSON %q: %v", stdout, err)
	}
}

func windowsTokenContractForwardedRequest(
	t *testing.T,
	baseURL string,
	token string,
) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, baseURL+"/api/sessions", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Forwarded-For", "198.51.100.24")
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(body)
}
