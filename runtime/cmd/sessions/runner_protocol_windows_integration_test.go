//go:build windows

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

const windowsRunnerProtocolChildEnv = "SESSIONS_TEST_RUNNER_PROTOCOL_CHILD"

func TestWindowsRunnerProtocolReAdoption(t *testing.T) {
	if os.Getenv(windowsRunnerProtocolChildEnv) != "" {
		t.Skip("parent assertion only")
	}

	moduleRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binaryRoot := t.TempDir()
	daemonBinary := filepath.Join(binaryRoot, "sessionsd.exe")
	runnerBinary := filepath.Join(binaryRoot, "sessions-runner.exe")
	buildWindowsTokenContractBinary(t, moduleRoot, daemonBinary, "./cmd/sessionsd")
	buildWindowsTokenContractBinary(t, moduleRoot, runnerBinary, "./cmd/sessions-runner")

	scratchRoot := t.TempDir()
	runnerStateDir := filepath.Join(scratchRoot, "runners")
	if err := os.MkdirAll(runnerStateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	port := reserveWindowsTokenContractPort(t)
	environment := append(
		windowsTokenContractEnvironment(scratchRoot, runnerStateDir, port),
		"SESSIONS_RUNNER="+runnerBinary,
	)
	baseURL := "http://127.0.0.1:" + port
	sessionID := "windows-protocol-" + strconv.Itoa(os.Getpid())

	runner := startWindowsRunnerProtocolProcess(
		t,
		runnerBinary,
		environment,
		runnerStateDir,
		sessionID,
	)
	metadata := waitForWindowsRunnerProtocolMetadata(t, runner, runnerStateDir, sessionID)
	runnerPID := runner.command.Process.Pid
	childPID := metadata.Info.PID
	assertWindowsRunnerProtocolProcessAlive(t, "runner", runnerPID)
	assertWindowsRunnerProtocolProcessAlive(t, "provider child", childPID)

	daemon := startWindowsTokenContractDaemon(t, daemonBinary, environment)
	waitForWindowsTokenContractHealth(t, daemon, baseURL)
	first := waitForWindowsRunnerProtocolSession(t, daemon, baseURL, sessionID)
	assertWindowsRunnerProtocolIdentity(t, first, childPID)
	waitForWindowsRunnerProtocolSnapshot(t, daemon, baseURL, sessionID, "PROTOCOL_READY")

	const beforeRestart = "MARKER_BEFORE_DAEMON_RESTART"
	postWindowsRunnerProtocolInput(t, baseURL, sessionID, beforeRestart+"\r")
	waitForWindowsRunnerProtocolSnapshot(
		t,
		daemon,
		baseURL,
		sessionID,
		"PROTOCOL:"+beforeRestart,
	)

	daemon.stop(t)
	assertWindowsRunnerProtocolProcessAlive(t, "runner after daemon stop", runnerPID)
	assertWindowsRunnerProtocolProcessAlive(t, "provider child after daemon stop", childPID)

	restarted := startWindowsTokenContractDaemon(t, daemonBinary, environment)
	waitForWindowsTokenContractHealth(t, restarted, baseURL)
	reattached := waitForWindowsRunnerProtocolSession(t, restarted, baseURL, sessionID)
	assertWindowsRunnerProtocolIdentity(t, reattached, childPID)
	assertWindowsRunnerProtocolProcessAlive(t, "runner after daemon restart", runnerPID)
	assertWindowsRunnerProtocolProcessAlive(t, "provider child after daemon restart", childPID)
	waitForWindowsRunnerProtocolSnapshot(
		t,
		restarted,
		baseURL,
		sessionID,
		"PROTOCOL:"+beforeRestart,
	)

	const afterRestart = "MARKER_AFTER_DAEMON_RESTART"
	postWindowsRunnerProtocolInput(t, baseURL, sessionID, afterRestart+"\r")
	waitForWindowsRunnerProtocolSnapshot(
		t,
		restarted,
		baseURL,
		sessionID,
		"PROTOCOL:"+afterRestart,
	)
}

func TestWindowsRunnerUnexpectedLossClassification(t *testing.T) {
	if os.Getenv(windowsRunnerProtocolChildEnv) != "" {
		t.Skip("parent assertion only")
	}

	moduleRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binaryRoot := t.TempDir()
	daemonBinary := filepath.Join(binaryRoot, "sessionsd.exe")
	runnerBinary := filepath.Join(binaryRoot, "sessions-runner.exe")
	buildWindowsTokenContractBinary(t, moduleRoot, daemonBinary, "./cmd/sessionsd")
	buildWindowsTokenContractBinary(t, moduleRoot, runnerBinary, "./cmd/sessions-runner")

	scratchRoot := t.TempDir()
	runnerStateDir := filepath.Join(scratchRoot, "runners")
	if err := os.MkdirAll(runnerStateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	port := reserveWindowsTokenContractPort(t)
	environment := append(
		windowsTokenContractEnvironment(scratchRoot, runnerStateDir, port),
		"SESSIONS_RUNNER="+runnerBinary,
	)
	baseURL := "http://127.0.0.1:" + port

	daemon := startWindowsTokenContractDaemon(t, daemonBinary, environment)
	waitForWindowsTokenContractHealth(t, daemon, baseURL)
	created := createWindowsRunnerProtocolSession(t, daemon, baseURL)
	paths := state.For(runnerStateDir, created.ID)
	metadata := waitForWindowsRunnerProtocolCreatedMetadata(t, daemon, paths.Meta, created.ID)
	providerPID := metadata.Info.PID
	runnerPID, runnerImage := windowsRunnerProtocolParentProcess(t, providerPID)
	if !strings.EqualFold(filepath.Base(runnerImage), filepath.Base(runnerBinary)) {
		t.Fatalf(
			"provider parent %d image = %q, want scratch runner %q",
			runnerPID,
			runnerImage,
			runnerBinary,
		)
	}
	runnerHandle := openWindowsRunnerProtocolProcess(
		t,
		"scratch runner",
		runnerPID,
		windows.PROCESS_TERMINATE|windows.SYNCHRONIZE,
	)
	providerHandle := openWindowsRunnerProtocolProcess(
		t,
		"scratch provider",
		providerPID,
		windows.PROCESS_TERMINATE|windows.SYNCHRONIZE,
	)
	t.Cleanup(func() {
		cleanupWindowsRunnerProtocolProcesses(runnerHandle, providerHandle)
	})

	waitForWindowsRunnerProtocolSnapshot(t, daemon, baseURL, created.ID, "PROTOCOL_READY")
	const marker = "MARKER_BEFORE_UNEXPECTED_RUNNER_LOSS"
	postWindowsRunnerProtocolInput(t, baseURL, created.ID, marker+"\r")
	waitForWindowsRunnerProtocolSnapshot(
		t,
		daemon,
		baseURL,
		created.ID,
		"PROTOCOL:"+marker,
	)
	metadataBeforeLoss, err := os.ReadFile(paths.Meta)
	if err != nil {
		t.Fatal(err)
	}
	waitForWindowsRunnerProtocolPersistentMarker(t, paths.Events, "PROTOCOL:"+marker)

	if err := windows.TerminateProcess(runnerHandle, 137); err != nil {
		t.Fatalf("terminate scratch runner %d: %v", runnerPID, err)
	}
	waitForWindowsRunnerProtocolHandleExit(t, "scratch runner", runnerHandle)
	waitForWindowsRunnerProtocolHandleExit(t, "scratch provider Job child", providerHandle)

	lost := waitForWindowsRunnerProtocolLostRecord(t, daemon, baseURL, created.ID)
	// The contract this pins changed deliberately: a runner killed under the
	// daemon leaves the session UNREACHABLE, never ended. Requiring Exited
	// here is what let a socket death present as a finished session, take its
	// spawned processes down with it, and hide it from the default listing.
	// Exited must stay false and carry no invented exit details, because no
	// status was ever reaped.
	if !lost.Unreachable ||
		lost.UnreachableReason != "runner-lost" ||
		lost.Exited ||
		lost.ExitCode != nil ||
		lost.EndedByKind != "" {
		t.Fatalf(
			"unexpected runner loss record = unreachable=%v reason=%q exited=%v code=%v endedBy=%q",
			lost.Unreachable,
			lost.UnreachableReason,
			lost.Exited,
			lost.ExitCode,
			lost.EndedByKind,
		)
	}
	report := readWindowsRunnerProtocolRecoveryReport(t, daemon, baseURL)
	var recovered *recovery.Lane
	for index := range report.Lanes {
		if report.Lanes[index].ID == created.ID {
			recovered = &report.Lanes[index]
			break
		}
	}
	if recovered == nil {
		t.Fatalf("recovery report omitted lost scratch session %s: %#v", created.ID, report.Lanes)
	}
	if recovered.Class != ledger.ClassUnexpectedlyLost ||
		!recovered.Reality.MetadataPresent ||
		recovered.Reality.ManagerVisible ||
		recovered.Reality.Hello {
		t.Fatalf("lost scratch recovery classification = %#v", *recovered)
	}

	// Let the first reconnect attempt run. It may attach only to the same live
	// runner; it must never launch a replacement for an unexpectedly lost one.
	time.Sleep(1500 * time.Millisecond)
	// The session stays in the DEFAULT listing, marked unreachable. It used to
	// vanish from it, which is the same mistake as reporting it exited: losing
	// the socket is Sessions losing contact, not the session ending, and a
	// record that disappears because a connection died is a kill wearing
	// sleep's clothes. What must not happen is a replacement runner being
	// launched, or the record claiming an exit nobody reaped.
	active := readWindowsRunnerProtocolSessions(t, daemon, baseURL, false)
	if len(active) != 1 ||
		active[0].ID != created.ID ||
		!active[0].Unreachable ||
		active[0].UnreachableReason != "runner-lost" ||
		active[0].Exited {
		t.Fatalf("an unreachably-lost session must stay listed and unended: %#v", active)
	}
	closed := readWindowsRunnerProtocolSessions(t, daemon, baseURL, true)
	if len(closed) != 1 ||
		closed[0].ID != created.ID ||
		!closed[0].Unreachable ||
		closed[0].UnreachableReason != "runner-lost" ||
		closed[0].Exited {
		t.Fatalf("durable lost-session list = %#v", closed)
	}

	metadataAfterLoss, err := os.ReadFile(paths.Meta)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(metadataAfterLoss, metadataBeforeLoss) {
		t.Fatalf(
			"unexpected runner loss rewrote durable metadata:\nbefore=%s\nafter=%s",
			metadataBeforeLoss,
			metadataAfterLoss,
		)
	}
	waitForWindowsRunnerProtocolPersistentMarker(t, paths.Events, "PROTOCOL:"+marker)
	for _, path := range []string{paths.Meta, paths.Events, paths.Log} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("durable runner artifact %s missing after loss: %v", path, err)
		}
	}
}

func TestWindowsRunnerProtocolChildHelper(t *testing.T) {
	if os.Getenv(windowsRunnerProtocolChildEnv) == "" {
		t.Skip("helper is launched only inside the production Windows runner")
	}
	fmt.Println("PROTOCOL_READY")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		fmt.Println("PROTOCOL:" + strings.TrimSpace(scanner.Text()))
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

func createWindowsRunnerProtocolSession(
	t *testing.T,
	daemon *windowsTokenContractDaemon,
	baseURL string,
) state.SessionInfo {
	t.Helper()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(state.CreateSessionRequest{
		Cmd:  testBinary,
		Args: []string{"-test.run=^TestWindowsRunnerProtocolChildHelper$", "-test.v"},
		Cwd:  filepath.Dir(testBinary),
		Cols: 100,
		Rows: 30,
		Env:  map[string]string{windowsRunnerProtocolChildEnv: "1"},
		Name: "Windows unexpected runner loss contract",
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+"/api/sessions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated {
		daemon.stop(t)
		t.Fatalf(
			"create scratch Windows session: status=%d body=%s\nlogs:\n%s",
			response.StatusCode,
			responseBody,
			daemon.logs.String(),
		)
	}
	var created state.SessionInfo
	if err := json.Unmarshal(responseBody, &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.PID <= 0 || created.Exited {
		t.Fatalf("created scratch Windows session = %#v", created)
	}
	return created
}

func waitForWindowsRunnerProtocolCreatedMetadata(
	t *testing.T,
	daemon *windowsTokenContractDaemon,
	metadataPath string,
	sessionID string,
) state.RunnerMetadata {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		metadata, err := state.ReadRunnerMetadata(metadataPath)
		if err == nil && metadata.Info.ID == sessionID && metadata.Info.PID > 0 {
			return metadata
		}
		lastErr = err
		time.Sleep(25 * time.Millisecond)
	}
	daemon.stop(t)
	t.Fatalf(
		"scratch runner did not publish metadata for %s: %v\nlogs:\n%s",
		sessionID,
		lastErr,
		daemon.logs.String(),
	)
	return state.RunnerMetadata{}
}

func windowsRunnerProtocolParentProcess(t *testing.T, childPID int) (int, string) {
	t.Helper()
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(snapshot)

	processes := make(map[uint32]windows.ProcessEntry32)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		t.Fatal(err)
	}
	for {
		processes[entry.ProcessID] = entry
		err = windows.Process32Next(snapshot, &entry)
		if err == windows.ERROR_NO_MORE_FILES {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	child, ok := processes[uint32(childPID)]
	if !ok || child.ParentProcessID == 0 {
		t.Fatalf("scratch provider %d has no discoverable parent process", childPID)
	}
	parent, ok := processes[child.ParentProcessID]
	if !ok {
		t.Fatalf(
			"scratch provider %d parent %d is absent from the process snapshot",
			childPID,
			child.ParentProcessID,
		)
	}
	return int(parent.ProcessID), windows.UTF16ToString(parent.ExeFile[:])
}

func openWindowsRunnerProtocolProcess(
	t *testing.T,
	label string,
	pid int,
	access uint32,
) windows.Handle {
	t.Helper()
	handle, err := windows.OpenProcess(access, false, uint32(pid))
	if err != nil {
		t.Fatalf("open %s process %d: %v", label, pid, err)
	}
	return handle
}

func waitForWindowsRunnerProtocolHandleExit(
	t *testing.T,
	label string,
	handle windows.Handle,
) {
	t.Helper()
	event, err := windows.WaitForSingleObject(handle, 5_000)
	if err != nil {
		t.Fatalf("wait for %s exit: %v", label, err)
	}
	if event != uint32(windows.WAIT_OBJECT_0) {
		t.Fatalf("%s did not exit: wait=%d", label, event)
	}
}

func cleanupWindowsRunnerProtocolProcesses(handles ...windows.Handle) {
	for _, handle := range handles {
		if handle == 0 || handle == windows.InvalidHandle {
			continue
		}
		if event, err := windows.WaitForSingleObject(handle, 0); err == nil &&
			event == uint32(windows.WAIT_TIMEOUT) {
			_ = windows.TerminateProcess(handle, 137)
			_, _ = windows.WaitForSingleObject(handle, 5_000)
		}
		_ = windows.CloseHandle(handle)
	}
}

func waitForWindowsRunnerProtocolPersistentMarker(
	t *testing.T,
	eventsPath string,
	marker string,
) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		events, err := state.Restore(eventsPath)
		if err == nil {
			for _, event := range events {
				if strings.Contains(string(event.Data), marker) {
					return
				}
			}
		}
		lastErr = err
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("persistent runner events did not contain %q: %v", marker, lastErr)
}

func waitForWindowsRunnerProtocolLostRecord(
	t *testing.T,
	daemon *windowsTokenContractDaemon,
	baseURL string,
	sessionID string,
) state.SessionInfo {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last []state.SessionInfo
	for time.Now().Before(deadline) {
		last = readWindowsRunnerProtocolSessions(t, daemon, baseURL, true)
		for _, session := range last {
			// Unreachable, not exited. Killing the runner destroys the way the
			// daemon talks to a session; it says nothing about whether the
			// session ended, and this contract used to require the daemon to
			// claim it had. See internal/liveness and the Unreachable split.
			if session.ID == sessionID &&
				session.Unreachable &&
				session.UnreachableReason == "runner-lost" {
				return session
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	daemon.stop(t)
	t.Fatalf(
		"scratch runner loss was not classified for %s: sessions=%#v\nlogs:\n%s",
		sessionID,
		last,
		daemon.logs.String(),
	)
	return state.SessionInfo{}
}

func readWindowsRunnerProtocolSessions(
	t *testing.T,
	daemon *windowsTokenContractDaemon,
	baseURL string,
	includeExited bool,
) []state.SessionInfo {
	t.Helper()
	path := baseURL + "/api/sessions"
	if includeExited {
		path += "?include_exited=1"
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Get(path)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		daemon.stop(t)
		t.Fatalf(
			"list scratch Windows sessions: status=%d body=%s\nlogs:\n%s",
			response.StatusCode,
			body,
			daemon.logs.String(),
		)
	}
	var payload struct {
		Sessions []state.SessionInfo `json:"sessions"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Sessions
}

func readWindowsRunnerProtocolRecoveryReport(
	t *testing.T,
	daemon *windowsTokenContractDaemon,
	baseURL string,
) recovery.Report {
	t.Helper()
	response, err := (&http.Client{Timeout: 5 * time.Second}).Get(baseURL + "/api/recovery")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		daemon.stop(t)
		t.Fatalf(
			"read scratch Windows recovery report: status=%d body=%s\nlogs:\n%s",
			response.StatusCode,
			body,
			daemon.logs.String(),
		)
	}
	var report recovery.Report
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	return report
}

type windowsRunnerProtocolProcess struct {
	command *exec.Cmd
	logs    bytes.Buffer
	stopped bool
}

func startWindowsRunnerProtocolProcess(
	t *testing.T,
	runnerBinary string,
	environment []string,
	runnerStateDir string,
	sessionID string,
) *windowsRunnerProtocolProcess {
	t.Helper()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := json.Marshal([]string{
		"-test.run=^TestWindowsRunnerProtocolChildHelper$",
		"-test.v",
	})
	if err != nil {
		t.Fatal(err)
	}
	process := &windowsRunnerProtocolProcess{}
	process.command = exec.Command(runnerBinary)
	process.command.Env = append(
		append([]string(nil), environment...),
		windowsRunnerProtocolChildEnv+"=1",
		"RUNNER_ID="+sessionID,
		"RUNNER_STATE_DIR="+runnerStateDir,
		"RUNNER_CMD="+testBinary,
		"RUNNER_ARGS_JSON="+string(arguments),
		"RUNNER_CWD="+filepath.Dir(testBinary),
		"RUNNER_COLS=100",
		"RUNNER_ROWS=30",
	)
	process.command.Stdout = &process.logs
	process.command.Stderr = &process.logs
	if err := process.command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		process.stop(t)
	})
	return process
}

func (p *windowsRunnerProtocolProcess) stop(t *testing.T) {
	t.Helper()
	if p == nil || p.stopped {
		return
	}
	p.stopped = true
	if p.command.Process != nil {
		_ = p.command.Process.Kill()
	}
	waited := make(chan error, 1)
	go func() {
		waited <- p.command.Wait()
	}()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatalf("scratch runner did not exit; logs:\n%s", p.logs.String())
	}
}

func waitForWindowsRunnerProtocolMetadata(
	t *testing.T,
	runner *windowsRunnerProtocolProcess,
	runnerStateDir string,
	sessionID string,
) state.RunnerMetadata {
	t.Helper()
	metadataPath := filepath.Join(runnerStateDir, sessionID+".json")
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		metadata, err := state.ReadRunnerMetadata(metadataPath)
		if err == nil && metadata.Info.ID == sessionID && metadata.Info.PID > 0 {
			return metadata
		}
		lastErr = err
		time.Sleep(25 * time.Millisecond)
	}
	runner.stop(t)
	t.Fatalf(
		"scratch runner did not publish metadata for %s: %v\nlogs:\n%s",
		sessionID,
		lastErr,
		runner.logs.String(),
	)
	return state.RunnerMetadata{}
}

func waitForWindowsRunnerProtocolSession(
	t *testing.T,
	daemon *windowsTokenContractDaemon,
	baseURL string,
	sessionID string,
) state.SessionInfo {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(20 * time.Second)
	var lastStatus int
	var lastBody string
	for time.Now().Before(deadline) {
		response, err := client.Get(baseURL + "/api/sessions")
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr == nil {
				lastStatus = response.StatusCode
				lastBody = string(body)
				var payload struct {
					Sessions []state.SessionInfo `json:"sessions"`
				}
				if response.StatusCode == http.StatusOK &&
					json.Unmarshal(body, &payload) == nil {
					for _, session := range payload.Sessions {
						if session.ID == sessionID && !session.Exited {
							return session
						}
					}
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	daemon.stop(t)
	t.Fatalf(
		"scratch daemon did not re-adopt %s: status=%d body=%s\nlogs:\n%s",
		sessionID,
		lastStatus,
		lastBody,
		daemon.logs.String(),
	)
	return state.SessionInfo{}
}

func assertWindowsRunnerProtocolIdentity(
	t *testing.T,
	session state.SessionInfo,
	childPID int,
) {
	t.Helper()
	if session.PID != childPID ||
		session.RunnerProtocol != proto.ProtocolVersion ||
		session.Exited {
		t.Fatalf(
			"re-adopted session identity = id=%s pid=%d protocol=%d exited=%v; want pid=%d protocol=%d live",
			session.ID,
			session.PID,
			session.RunnerProtocol,
			session.Exited,
			childPID,
			proto.ProtocolVersion,
		)
	}
}

func postWindowsRunnerProtocolInput(
	t *testing.T,
	baseURL string,
	sessionID string,
	data string,
) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"data": data})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(
		http.MethodPost,
		baseURL+"/api/sessions/"+sessionID+"/input",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf(
			"send scratch runner input: status=%d body=%s",
			response.StatusCode,
			responseBody,
		)
	}
}

func waitForWindowsRunnerProtocolSnapshot(
	t *testing.T,
	daemon *windowsTokenContractDaemon,
	baseURL string,
	sessionID string,
	marker string,
) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(15 * time.Second)
	var lastStatus int
	var lastBody string
	for time.Now().Before(deadline) {
		response, err := client.Get(baseURL + "/api/sessions/" + sessionID + "/snapshot")
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr == nil {
				lastStatus = response.StatusCode
				lastBody = string(body)
				if response.StatusCode == http.StatusOK &&
					strings.Contains(lastBody, marker) {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	daemon.stop(t)
	t.Fatalf(
		"scratch runner snapshot did not contain %q: status=%d body=%q\nlogs:\n%s",
		marker,
		lastStatus,
		lastBody,
		daemon.logs.String(),
	)
}

func assertWindowsRunnerProtocolProcessAlive(t *testing.T, label string, pid int) {
	t.Helper()
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatalf("open %s process %d: %v", label, pid, err)
	}
	defer windows.CloseHandle(handle)
	event, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		t.Fatalf("inspect %s process %d: %v", label, pid, err)
	}
	if event != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("%s process %d is not alive: wait=%d", label, pid, event)
	}
}
