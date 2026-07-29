package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

const launchdHelperEnvironment = "SESSIONS_TEST_LAUNCHD_HELPER"

func TestDaemonScratchLaunchdHelper(t *testing.T) {
	if os.Getenv(launchdHelperEnvironment) != "1" {
		return
	}
	address := net.JoinHostPort(os.Getenv("SESSIONS_HOST"), os.Getenv("SESSIONS_PORT"))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/health" {
			http.NotFound(response, request)
			return
		}
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte(`{"ok":true}`))
	})}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	select {
	case <-signals:
		_ = server.Close()
	case err := <-serveDone:
		if err != nil && err != http.ErrServerClosed {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("launchd helper timed out waiting for bootout")
	}
}

func TestDaemonScratchLaunchdBootstrapHealthBootout(t *testing.T) {
	if testing.Short() {
		t.Skip("scratch launchd lifecycle is an integration test")
	}
	launchctl := launchctlExecutable()
	if launchctl == "" {
		t.Skip("launchctl is unavailable")
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	if output, err := exec.Command(launchctl, "print", domain).CombinedOutput(); err != nil {
		t.Skipf("launchd GUI domain is unavailable: %s", outputOrError(output, err))
	}

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(probe.Addr().(*net.TCPAddr).Port)
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}

	directory := t.TempDir()
	label := fmt.Sprintf("tech.somewhere.sessions.dev.daemon.scratch.%d.%d", os.Getpid(), time.Now().UnixNano())
	agentsDir := filepath.Join(directory, "Library", "LaunchAgents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	plistPath := filepath.Join(agentsDir, label+".plist")
	logPath := filepath.Join(directory, "daemon.log")
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	xml := daemonPlist(daemonPlistOptions{
		Label: label,
		ProgramArguments: []string{
			testBinary,
			"-test.run=^TestDaemonScratchLaunchdHelper$",
		},
		WorkingDir: directory,
		LogFile:    logPath,
		Env: []plistEnvironment{
			{Key: "PATH", Value: "/usr/bin:/bin"},
			{Key: "SESSIONS_HOST", Value: "127.0.0.1"},
			{Key: "SESSIONS_PORT", Value: port},
			{Key: launchdHelperEnvironment, Value: "1"},
		},
	})
	if err := writeDaemonPlist(plistPath, xml); err != nil {
		t.Fatal(err)
	}
	serviceTarget := domain + "/" + label
	cleanup := func() {
		_, _ = exec.Command(launchctl, "bootout", serviceTarget).CombinedOutput()
		_ = os.Remove(plistPath)
	}
	t.Cleanup(cleanup)

	if output, err := exec.Command(launchctl, "bootstrap", domain, plistPath).CombinedOutput(); err != nil {
		t.Fatalf("bootstrap %s: %s\n%s", label, outputOrError(output, err), readTestLog(logPath))
	}
	healthURL := "http://127.0.0.1:" + port + "/api/health"
	deadline := time.Now().Add(10 * time.Second)
	lastError := "no response"
	for time.Now().Before(deadline) {
		response, requestErr := http.Get(healthURL)
		if requestErr == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				lastError = ""
				break
			}
			lastError = response.Status
		} else {
			lastError = requestErr.Error()
		}
		time.Sleep(100 * time.Millisecond)
	}
	if lastError != "" {
		t.Fatalf("scratch service unhealthy at %s: %s\n%s", healthURL, lastError, readTestLog(logPath))
	}
	t.Setenv("SESSIONS_DAEMON_LABEL", label)
	application := &app{home: directory, stdout: io.Discard}
	if err := application.cmdUninstall(nil); err != nil {
		t.Fatalf("first uninstall %s: %s\n%s", label, err, readTestLog(logPath))
	}
	if err := application.cmdUninstall(nil); err != nil {
		t.Fatalf("idempotent uninstall %s: %s\n%s", label, err, readTestLog(logPath))
	}
	if output, err := exec.Command(launchctl, "print", serviceTarget).CombinedOutput(); err == nil {
		t.Fatalf("scratch service remained loaded after bootout: %s", output)
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Fatalf("scratch plist remained after uninstall: %v", err)
	}
	t.Logf("scratch lifecycle passed: label=%s health=200 uninstall=clean+idempotent", label)
}

func readTestLog(path string) string {
	contents, err := os.ReadFile(path)
	if err != nil {
		return "launchd log unavailable: " + err.Error()
	}
	return string(contents)
}
