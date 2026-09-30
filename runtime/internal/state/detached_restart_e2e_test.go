//go:build !windows

package state_test

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The promise this launcher exists to keep: a session outlives the daemon.
//
// On macOS launchd owns the runner, so the daemon can die without touching it.
// On every other Unix there was no launcher at all — `launchctl bootstrap …:
// executable file not found in $PATH` — and the detached launcher that
// replaces it starts the runner itself. A launcher that started runners as
// ordinary children would pass every unit test in this package and still lose
// every session the moment the daemon was restarted or its process group was
// signalled, which is what this test refuses to allow.
//
// It runs the real binaries against an isolated state directory: a daemon, a
// headless lane counting to twenty, a SIGKILL to the daemon's whole process
// group while the lane is mid-count, a second daemon, and then the lane's own
// output — every record, exactly once.
func TestDetachedRunnerSurvivesADaemonRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries and waits on a twenty-second lane")
	}
	// Short root: a Unix socket path is bounded, and the runner appends a UUID.
	root, err := os.MkdirTemp("/tmp", "sessions-detached-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	fleet := buildSessionsBinaries(t, root)
	environment := isolatedDaemonEnvironment(t, root, fleet)

	daemon := startDaemon(t, fleet, environment)
	waitForDaemon(t, environment.port)

	lane := runCLI(t, fleet.cli, environment.values, "run", "--name", "counting", "--cwd", root,
		"--", "sh", "-c", "for i in $(seq 1 20); do echo $i; sleep 1; done")
	laneID := strings.TrimSpace(lane)
	if laneID == "" {
		t.Fatal("sessions run printed no lane id")
	}
	t.Logf("lane %s", laneID)

	runnerPID := waitForRunnerPID(t, environment.runnerDir, laneID)
	waitForLaneProgress(t, environment.runnerDir, laneID)

	// Kill the daemon's whole process group, the way a service manager, a
	// shell, or a crashing supervisor would. A runner started as an ordinary
	// child shares that group and dies here.
	if err := syscall.Kill(-daemon.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill daemon group: %v", err)
	}
	_ = daemon.Wait()
	if !processAlive(runnerPID) {
		t.Fatalf("runner %d died with the daemon; the session did not outlive it", runnerPID)
	}

	restarted := startDaemon(t, fleet, environment)
	t.Cleanup(func() {
		_ = syscall.Kill(-restarted.Process.Pid, syscall.SIGKILL)
		_ = restarted.Wait()
	})
	waitForDaemon(t, environment.port)

	// The second daemon re-adopted the runner: it can answer for the lane, and
	// it delivers the lane's completion when the command finishes.
	completion := runCLI(t, fleet.cli, environment.values, "--json", "wait", laneID, "--timeout", "90s")
	var envelope struct {
		OK   bool `json:"ok"`
		Lane struct {
			ExitCode       int    `json:"exit_code"`
			LastOutputTail string `json:"last_output_tail"`
		} `json:"lane"`
	}
	if err := json.Unmarshal([]byte(completion), &envelope); err != nil {
		t.Fatalf("decode wait envelope: %v\n%s", err, completion)
	}
	if !envelope.OK || envelope.Lane.ExitCode != 0 {
		t.Fatalf("lane did not finish cleanly: %s", completion)
	}
	assertCountedOnce(t, envelope.Lane.LastOutputTail, 20)
}

// assertCountedOnce is the whole point: not "some output survived" but every
// record exactly once. A lost daemon must cost neither a line nor a duplicate.
func assertCountedOnce(t *testing.T, output string, count int) {
	t.Helper()
	seen := make(map[string]int, count)
	for _, line := range strings.Fields(output) {
		seen[line]++
	}
	for number := 1; number <= count; number++ {
		switch seen[strconv.Itoa(number)] {
		case 1:
		case 0:
			t.Fatalf("record %d never arrived; output was:\n%s", number, output)
		default:
			t.Fatalf("record %d arrived %d times; output was:\n%s", number, seen[strconv.Itoa(number)], output)
		}
	}
}

type sessionsBinaries struct{ daemon, cli, runner string }

type daemonEnvironment struct {
	values     []string
	port       int
	runnerDir  string
	stateDir   string
	homeFolder string
}

func buildSessionsBinaries(t *testing.T, root string) sessionsBinaries {
	t.Helper()
	moduleRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	built := sessionsBinaries{
		daemon: filepath.Join(root, "sessionsd"),
		cli:    filepath.Join(root, "sessions"),
		runner: filepath.Join(root, "sessions-runner"),
	}
	for target, output := range map[string]string{
		"./cmd/sessionsd":       built.daemon,
		"./cmd/sessions":        built.cli,
		"./cmd/sessions-runner": built.runner,
	} {
		build := exec.Command("go", "build", "-o", output, target)
		build.Dir = moduleRoot
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if combined, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", target, err, combined)
		}
	}
	return built
}

// isolatedDaemonEnvironment keeps this test entirely off the machine's own
// Sessions state: its own HOME, state directory, ledger, and port.
func isolatedDaemonEnvironment(t *testing.T, root string, fleet sessionsBinaries) daemonEnvironment {
	t.Helper()
	home := filepath.Join(root, "home")
	stateDir := filepath.Join(root, "state")
	for _, directory := range []string{home, stateDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	port := freePort(t)
	return daemonEnvironment{
		port: port, runnerDir: stateDir, stateDir: stateDir, homeFolder: home,
		values: append(environmentWithoutSessionsAncestry(),
			"HOME="+home,
			"SESSIONS_STATE_DIR="+stateDir,
			"SESSIONS_LEDGER_PATH="+filepath.Join(root, "ledger.sqlite3"),
			"SESSIONS_PORT="+strconv.Itoa(port),
			"SESSIONS_HOST=127.0.0.1",
			"SESSIONS_RUNNER="+fleet.runner,
			"SESSIONS_LAUNCHER=detached",
		),
	}
}

// environmentWithoutSessionsAncestry drops the variables a Sessions session
// exports into anything it runs. Inherited, they would make this test's CLI
// claim to be a child of whatever session started the test — a creator id that
// does not exist in this isolated ledger.
func environmentWithoutSessionsAncestry() []string {
	kept := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		switch {
		case strings.HasPrefix(entry, "SESSIONS_SESSION_ID="),
			strings.HasPrefix(entry, "SESSIONS_OWNER_ID="),
			strings.HasPrefix(entry, "SESSIONS_STATE_DIR="),
			strings.HasPrefix(entry, "SESSIONS_LEDGER_PATH="),
			strings.HasPrefix(entry, "SESSIONS_PORT="),
			strings.HasPrefix(entry, "SESSIONS_HOST="),
			strings.HasPrefix(entry, "SESSIONS_RUNNER="),
			strings.HasPrefix(entry, "RUNNER_ID="):
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

func startDaemon(t *testing.T, fleet sessionsBinaries, environment daemonEnvironment) *exec.Cmd {
	t.Helper()
	logPath := filepath.Join(environment.stateDir, fmt.Sprintf("daemon-%d.log", time.Now().UnixNano()))
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = logFile.Close()
		if t.Failed() {
			if contents, readErr := os.ReadFile(logPath); readErr == nil {
				t.Logf("daemon log %s:\n%s", filepath.Base(logPath), contents)
			}
		}
	})
	command := exec.Command(fleet.daemon, "--serve", "--remote-auto-preview")
	command.Env = environment.values
	command.Dir = environment.homeFolder
	command.Stdout = logFile
	command.Stderr = logFile
	// Its own process group, so the test can end the daemon the way a service
	// manager would — and so ending it cannot reach a runner that detached.
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	return command
}

func waitForDaemon(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	address := fmt.Sprintf("http://127.0.0.1:%d/api/health", port)
	for time.Now().Before(deadline) {
		response, err := http.Get(address) //nolint:gosec,noctx // loopback health probe in a test
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("daemon on port %d never answered /api/health", port)
}

// waitForRunnerPID reads the pid out of the launch record the detached
// launcher wrote, which is also what a later daemon reads to know which
// process belonged to this session.
func waitForRunnerPID(t *testing.T, runnerDir, id string) int {
	t.Helper()
	path := filepath.Join(runnerDir, id+".launch.json")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			var record struct {
				PID int `json:"pid"`
			}
			if json.Unmarshal(raw, &record) == nil && record.PID > 0 {
				return record.PID
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no launch record with a pid at %s", path)
	return 0
}

// waitForLaneProgress waits until the lane has actually produced output, so the
// daemon is killed mid-count rather than before the work started.
func waitForLaneProgress(t *testing.T, runnerDir, id string) {
	t.Helper()
	path := filepath.Join(runnerDir, id+".events")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if contents, err := os.ReadFile(path); err == nil && strings.Contains(string(contents), "3") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the lane produced no output within 30s (%s)", path)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	return err == nil && process.Signal(syscall.Signal(0)) == nil
}

func runCLI(t *testing.T, binary string, environment []string, args ...string) string {
	t.Helper()
	command := exec.Command(binary, args...)
	command.Env = environment
	output, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		stderr := ""
		if ok := asExitError(err, &exit); ok {
			stderr = string(exit.Stderr)
		}
		t.Fatalf("sessions %s: %v\nstdout=%s\nstderr=%s", strings.Join(args, " "), err, output, stderr)
	}
	return string(output)
}

func asExitError(err error, target **exec.ExitError) bool {
	exit, ok := err.(*exec.ExitError) //nolint:errorlint // the concrete type is what carries Stderr
	if ok {
		*target = exit
	}
	return ok
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
