package interop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

type interopToolchain struct {
	repoRoot   string
	buildRoot  string
	goDaemon   string
	goSessions string
	goRunner   string
	node       string
	nodeDaemon string
	nodeRunner string
}

var tools interopToolchain

func TestMain(m *testing.M) {
	if runtime.GOOS != "darwin" {
		os.Exit(m.Run())
	}

	repoRoot, err := findRepoRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cutover interop setup: %v\n", err)
		os.Exit(1)
	}
	buildRoot, err := os.MkdirTemp("/tmp", "sessions-cutover-build-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "cutover interop setup: %v\n", err)
		os.Exit(1)
	}

	node, err := exec.LookPath("node")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cutover interop setup: node is required")
		_ = os.RemoveAll(buildRoot)
		os.Exit(1)
	}
	if err := runBuild(repoRoot, envWith(os.Environ(), map[string]string{}), "npm", "--prefix", "runtime/testdata/node-runtime", "run", "build"); err != nil {
		fmt.Fprintf(os.Stderr, "cutover interop setup: %v\n", err)
		_ = os.RemoveAll(buildRoot)
		os.Exit(1)
	}

	goRoot := filepath.Join(repoRoot, "runtime")
	goEnv := envWith(os.Environ(), map[string]string{"CGO_ENABLED": "0"})
	goDaemon := filepath.Join(buildRoot, "sessionsd")
	goSessions := filepath.Join(buildRoot, "sessions")
	goRunner := filepath.Join(buildRoot, "sessions-runner")
	if err := runBuild(goRoot, goEnv, "go", "build", "-o", goDaemon, "./cmd/sessionsd"); err != nil {
		fmt.Fprintf(os.Stderr, "cutover interop setup: %v\n", err)
		_ = os.RemoveAll(buildRoot)
		os.Exit(1)
	}
	if err := runBuild(goRoot, goEnv, "go", "build", "-o", goSessions, "./cmd/sessions"); err != nil {
		fmt.Fprintf(os.Stderr, "cutover interop setup: %v\n", err)
		_ = os.RemoveAll(buildRoot)
		os.Exit(1)
	}
	if err := runBuild(goRoot, goEnv, "go", "build", "-o", goRunner, "./cmd/sessions-runner"); err != nil {
		fmt.Fprintf(os.Stderr, "cutover interop setup: %v\n", err)
		_ = os.RemoveAll(buildRoot)
		os.Exit(1)
	}

	tools = interopToolchain{
		repoRoot:   repoRoot,
		buildRoot:  buildRoot,
		goDaemon:   goDaemon,
		goSessions: goSessions,
		goRunner:   goRunner,
		node:       node,
		nodeDaemon: filepath.Join(repoRoot, "runtime", "testdata", "node-runtime", "dist", "server.js"),
		nodeRunner: filepath.Join(repoRoot, "runtime", "testdata", "node-runtime", "dist", "runner.js"),
	}
	code := m.Run()
	_ = os.RemoveAll(buildRoot)
	os.Exit(code)
}

func TestNodeRunnerUnderGoDaemonCutover(t *testing.T) {
	testNodeRunnerUnderGoDaemonCutover(t, false)
}

func TestNodeRunnerUnderGoDaemonSymlinkCutover(t *testing.T) {
	testNodeRunnerUnderGoDaemonCutover(t, true)
}

func testNodeRunnerUnderGoDaemonCutover(t *testing.T, useCompatibilityLink bool) {
	requireDarwin(t)
	scratch := newScratch(t, "node-to-go")
	id := newUUID(t)

	runner := startProcess(t, "node-runner", scratch.root, scratch.env(map[string]string{
		"RUNNER_ID":        id,
		"RUNNER_STATE_DIR": scratch.runnerDir,
		"RUNNER_CMD":       "/bin/bash",
		"RUNNER_ARGS_JSON": `["-i"]`,
		"RUNNER_CWD":       scratch.workDir,
		"RUNNER_COLS":      "120",
		"RUNNER_ROWS":      "40",
	}), tools.node, tools.nodeRunner)
	t.Cleanup(func() { runner.stop(t, false) })
	waitForRunnerState(t, runner, scratch.runnerDir, id)

	daemonRunnerDir := scratch.runnerDir
	if useCompatibilityLink {
		sessionsState := filepath.Join(scratch.root, "sessions-state")
		if err := os.MkdirAll(sessionsState, 0o700); err != nil {
			t.Fatal(err)
		}
		daemonRunnerDir = filepath.Join(sessionsState, "runners")
		if err := os.Symlink(scratch.runnerDir, daemonRunnerDir); err != nil {
			t.Fatal(err)
		}
	}

	port := unusedPort(t)
	daemonEnv := scratch.env(map[string]string{
		"SESSIONS_HOST":        "127.0.0.1",
		"SESSIONS_PORT":        fmt.Sprint(port),
		"SESSIONS_STATE_DIR":   daemonRunnerDir,
		"SESSIONS_RUNNER":      tools.goRunner,
		"SESSIONS_LEDGER_PATH": filepath.Join(scratch.root, "ledger", "lanes.sqlite3"),
	})
	daemon := startProcess(t, "go-daemon", scratch.root, daemonEnv, tools.goDaemon)
	t.Cleanup(func() { daemon.stop(t, false) })
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForSession(t, baseURL, id)
	firstMarker := roundTrip(t, baseURL, id, "NODE_TO_GO")

	daemon.stop(t, true)
	assertRunnerSurvived(t, runner, filepath.Join(scratch.runnerDir, id+".sock"))

	restarted := startProcess(t, "go-daemon-restarted", scratch.root, daemonEnv, tools.goDaemon)
	t.Cleanup(func() { restarted.stop(t, false) })
	waitForSession(t, baseURL, id)
	secondMarker := roundTrip(t, baseURL, id, "NODE_TO_GO_REATTACHED")

	t.Logf(
		"node runner discovered by Go daemon: id=%s compatibility_link=%t first=%s after_restart=%s",
		id,
		useCompatibilityLink,
		firstMarker,
		secondMarker,
	)
}

func TestGoRunnerUnderNodeDaemonRegression(t *testing.T) {
	requireDarwin(t)
	scratch := newScratch(t, "go-to-node")
	id := newUUID(t)

	runner := startProcess(t, "go-runner", scratch.root, scratch.env(map[string]string{
		"RUNNER_ID":        id,
		"RUNNER_STATE_DIR": scratch.runnerDir,
		"RUNNER_CMD":       "/bin/bash",
		"RUNNER_ARGS_JSON": `["-i"]`,
		"RUNNER_CWD":       scratch.workDir,
		"RUNNER_COLS":      "120",
		"RUNNER_ROWS":      "40",
	}), tools.goRunner)
	t.Cleanup(func() { runner.stop(t, false) })
	waitForRunnerState(t, runner, scratch.runnerDir, id)

	port := unusedPort(t)
	daemon := startProcess(t, "node-daemon", scratch.root, scratch.env(map[string]string{
		"PRETTYD_HOST":      "127.0.0.1",
		"PRETTYD_PORT":      fmt.Sprint(port),
		"PRETTYD_STATE_DIR": scratch.runnerDir,
	}), tools.node, tools.nodeDaemon)
	t.Cleanup(func() { daemon.stop(t, false) })
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForSession(t, baseURL, id)
	marker := roundTrip(t, baseURL, id, "GO_TO_NODE")

	daemon.stop(t, true)
	assertRunnerSurvived(t, runner, filepath.Join(scratch.runnerDir, id+".sock"))
	t.Logf("Go runner discovered by node daemon: id=%s marker=%s", id, marker)
}

func TestGoManagerAdoptsFiveScratchRunnersAtOnceWithoutReaping(t *testing.T) {
	requireDarwin(t)
	scratch := newScratch(t, "scale")
	const runnerCount = 5
	runners := make([]*managedProcess, 0, runnerCount)
	ids := make([]string, 0, runnerCount)
	for index := 0; index < runnerCount; index++ {
		id := fmt.Sprintf("scratch-scale-%d-%d", os.Getpid(), index)
		runner := startScratchGoRunner(t, scratch, id, "/bin/bash", []string{"-i"})
		runners = append(runners, runner)
		ids = append(ids, id)
		writeAgedScratchPlist(t, scratch, id)
	}

	launcher := newScratchSocketLauncher(scratch)
	manager := session.NewManager(interopConfig(scratch), launcher, session.ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		DiscoveryRetries: 1, DiscoveryDelay: time.Millisecond,
	})
	t.Cleanup(manager.Close)
	if err := manager.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	attached, reaped := launcher.counts()
	if attached != runnerCount || len(manager.List(false)) != runnerCount || len(reaped) != 0 {
		t.Fatalf("scale discovery: attached=%d sessions=%d reaped=%v", attached, len(manager.List(false)), reaped)
	}
	for index, id := range ids {
		if _, present := manager.Get(id); !present {
			t.Errorf("scale runner %s was not registered", id)
		}
		assertRunnerSurvived(t, runners[index], filepath.Join(scratch.runnerDir, id+".sock"))
	}
	t.Logf("scale discovery attached=%d live=%d reaped=%d", attached, len(manager.List(false)), len(reaped))
}

func TestGuardedDiscoveryReapsOnlyFourDeadScratchArtifacts(t *testing.T) {
	requireDarwin(t)
	scratch := newScratch(t, "orphan-guard")
	const liveCount = 2
	const deadCount = session.DefaultMassKillLimit + 1
	liveIDs := make([]string, 0, liveCount)
	liveRunners := make([]*managedProcess, 0, liveCount)
	for index := 0; index < liveCount; index++ {
		id := fmt.Sprintf("scratch-orphan-live-%d-%d", os.Getpid(), index)
		liveIDs = append(liveIDs, id)
		liveRunners = append(liveRunners, startScratchGoRunner(t, scratch, id, "/bin/bash", []string{"-i"}))
		writeAgedScratchPlist(t, scratch, id)
	}

	deadIDs := make([]string, 0, deadCount)
	for index := 0; index < deadCount; index++ {
		id := fmt.Sprintf("scratch-orphan-dead-%d-%d", os.Getpid(), index)
		deadIDs = append(deadIDs, id)
		writeDeadScratchMetadata(t, scratch, id, exitedScratchPID(t))
		writeAgedScratchPlist(t, scratch, id)
	}

	launcher := newScratchSocketLauncher(scratch)
	manager := session.NewManager(interopConfig(scratch), launcher, session.ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		DiscoveryRetries: 1, DiscoveryDelay: time.Millisecond,
	})
	t.Cleanup(manager.Close)
	err := manager.Discover(context.Background())
	var guardErr *session.MassKillError
	if !errors.As(err, &guardErr) || guardErr.Count != deadCount {
		t.Fatalf("guarded discovery error = %v, want %d-candidate mass-kill refusal", err, deadCount)
	}
	attached, reaped := launcher.counts()
	if attached != liveCount || len(reaped) != 0 {
		t.Fatalf("guarded pass attached=%d reaped=%v", attached, reaped)
	}
	assertScratchArtifactsPresent(t, scratch, deadIDs)
	for index, id := range liveIDs {
		if _, present := manager.Get(id); !present {
			t.Fatalf("guarded pass did not attach live runner %s", id)
		}
		assertRunnerSurvived(t, liveRunners[index], filepath.Join(scratch.runnerDir, id+".sock"))
	}

	if err := manager.DiscoverWithOptions(context.Background(), session.DiscoverOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	_, reaped = launcher.counts()
	sort.Strings(reaped)
	sort.Strings(deadIDs)
	if strings.Join(reaped, "\n") != strings.Join(deadIDs, "\n") {
		t.Fatalf("forced pass reaped %v, want only %v", reaped, deadIDs)
	}
	for _, id := range deadIDs {
		for _, path := range []string{
			filepath.Join(scratch.runnerDir, id+".json"),
			state.RunnerPlistPath(scratch.launchAgentsDir, id),
		} {
			if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("forced pass left dead artifact %s: %v", path, statErr)
			}
		}
	}
	for index, id := range liveIDs {
		assertRunnerSurvived(t, liveRunners[index], filepath.Join(scratch.runnerDir, id+".sock"))
		if _, statErr := os.Stat(state.RunnerPlistPath(scratch.launchAgentsDir, id)); statErr != nil {
			t.Fatalf("forced pass removed live scratch plist %s: %v", id, statErr)
		}
	}
	t.Logf("orphan discovery guarded=%d forced_reaps=%d live_preserved=%d", guardErr.Count, len(reaped), liveCount)
}

func TestRealClaudeRunnerReattachesAndReResolvesStructuredHistory(t *testing.T) {
	requireDarwin(t)
	if os.Getenv("SESSIONS_INTEROP_REAL_CLAUDE") != "1" {
		t.Skip("set SESSIONS_INTEROP_REAL_CLAUDE=1 to run the authenticated provider interop")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal("real claude executable is required")
	}
	realHome := os.Getenv("HOME")
	if realHome == "" {
		t.Fatal("HOME is required for the authenticated Claude session")
	}
	scratch := newScratch(t, "real-claude")
	cleanupScratchClaudeProjects(t, realHome, scratch.workDir)
	id := newUUID(t)
	firstMarker := "CLAUDE_BEFORE_RESTART_" + strings.ToUpper(randomHex(t, 4))
	args := []string{
		"--session-id", id, "--dangerously-skip-permissions", "--tools", "",
		"--prompt-suggestions", "false", "--no-chrome",
	}
	runner := startScratchGoRunnerWithEnv(t, scratch, id, claude, args, map[string]string{
		"HOME":                realHome,
		"USER":                os.Getenv("USER"),
		"DISABLE_AUTOUPDATER": "1",
	})

	port := unusedPort(t)
	daemonEnv := scratch.env(map[string]string{
		"HOME":                        realHome,
		"USER":                        os.Getenv("USER"),
		"SESSIONS_HOST":               "127.0.0.1",
		"SESSIONS_PORT":               fmt.Sprint(port),
		"SESSIONS_STATE_DIR":          scratch.runnerDir,
		"SESSIONS_RUNNER":             tools.goRunner,
		"SESSIONS_LEDGER_PATH":        filepath.Join(scratch.root, "ledger", "lanes.sqlite3"),
		"SESSIONS_DISCOVERY_INTERVAL": "100ms",
	})
	daemon := startProcess(t, "go-daemon-claude", scratch.root, daemonEnv, tools.goDaemon)
	t.Cleanup(func() { daemon.stop(t, false) })
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	provider := waitForSessionMetadata(t, baseURL, id)
	if filepath.Base(provider.Cmd) != "claude" || provider.Tool != state.ToolClaude {
		t.Fatalf("provider metadata mismatch: %#v", provider)
	}
	runSessions(t, daemonEnv, scratch.workDir, "--json", "send", id, "--no-wait", "Reply with exactly "+firstMarker+" and no other text.")
	waitForClaudeSubmission(t, baseURL, id, firstMarker)
	waitForClaudeJSONL(t, realHome, scratch.workDir, id, baseURL, runner)
	before := waitForSessionsLast(t, daemonEnv, scratch.workDir, id, firstMarker)

	daemon.stop(t, true)
	assertRunnerSurvived(t, runner, filepath.Join(scratch.runnerDir, id+".sock"))
	restarted := startProcess(t, "go-daemon-claude-restarted", scratch.root, daemonEnv, tools.goDaemon)
	t.Cleanup(func() { restarted.stop(t, false) })
	waitForSessionMetadata(t, baseURL, id)
	afterRestart := waitForSessionsLast(t, daemonEnv, scratch.workDir, id, firstMarker)

	secondMarker := "CLAUDE_AFTER_RESTART_" + strings.ToUpper(randomHex(t, 4))
	runSessions(t, daemonEnv, scratch.workDir, "--json", "send", id, "--no-wait", "Reply with exactly "+secondMarker+" and no other text.")
	waitForClaudeSubmission(t, baseURL, id, secondMarker)
	afterResume := waitForSessionsLast(t, daemonEnv, scratch.workDir, id, secondMarker)
	if !strings.Contains(before, firstMarker) || !strings.Contains(afterRestart, firstMarker) || !strings.Contains(afterResume, secondMarker) {
		t.Fatal("sessions last did not retain and resume the structured Claude history")
	}
	t.Logf("real Claude structured history survived daemon restart: id=%s before=%s after=%s", id, firstMarker, secondMarker)
}

type scratchState struct {
	root            string
	home            string
	workDir         string
	runnerDir       string
	launchAgentsDir string
}

func newScratch(t *testing.T, name string) scratchState {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	state := scratchState{
		root: root, home: filepath.Join(root, "home"), workDir: filepath.Join(root, "work"),
	}
	state.runnerDir = filepath.Join(root, "state", "runners")
	state.launchAgentsDir = filepath.Join(root, "LaunchAgents")
	for _, dir := range []string{state.home, state.workDir, state.runnerDir, state.launchAgentsDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Keep HTTP authorization out of this protocol test while retaining the
	// exact state-root behavior both daemons use in production.
	openPaths := []string{
		filepath.Join(filepath.Dir(state.runnerDir), "open"),
		filepath.Join(state.home, ".local", "state", "sessions", "open"),
		// The archived Node cutover fixture predates the Sessions state root.
		filepath.Join(state.home, ".local", "state", "pretty-PTY", "open"),
	}
	for _, path := range openPaths {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("scratch %s state: %s", name, root)
	return state
}

func (s scratchState) env(overrides map[string]string) []string {
	base := map[string]string{
		"HOME":  s.home,
		"USER":  "sessions-cutover-test",
		"LANG":  "en_US.UTF-8",
		"SHELL": "/bin/bash",
	}
	for key, value := range overrides {
		base[key] = value
	}
	// Interop tests commonly run inside a Sessions-owned session. Never let the
	// parent runner's control plane (especially RUNNER_KIND) change the scratch
	// runner under test; only the explicit overrides above are authoritative.
	inherited := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if found && !strings.HasPrefix(key, "RUNNER_") {
			inherited = append(inherited, entry)
		}
	}
	return envWith(inherited, base)
}

type scratchSocketLauncher struct {
	scratch scratchState

	mu       sync.Mutex
	attached int
	reaped   []string
}

func newScratchSocketLauncher(scratch scratchState) *scratchSocketLauncher {
	return &scratchSocketLauncher{scratch: scratch}
}

func (l *scratchSocketLauncher) ProgramArguments(proto.LaunchRequest) []string {
	return []string{tools.goRunner}
}

func (l *scratchSocketLauncher) Launch(context.Context, proto.LaunchRequest) (proto.Runner, error) {
	return nil, errors.New("scratch socket launcher does not launch processes")
}

func (l *scratchSocketLauncher) Attach(ctx context.Context, info proto.RunnerInfo) (proto.Runner, error) {
	runner, err := proto.DialRunner(ctx, info.SocketPath)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.attached++
	l.mu.Unlock()
	return runner, nil
}

func (l *scratchSocketLauncher) Reap(id string) error {
	path := state.RunnerPlistPath(l.scratch.launchAgentsDir, id)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	l.mu.Lock()
	l.reaped = append(l.reaped, id)
	l.mu.Unlock()
	return nil
}

func (l *scratchSocketLauncher) counts() (int, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.attached, append([]string(nil), l.reaped...)
}

func interopConfig(scratch scratchState) state.Config {
	return state.Config{
		Host: "127.0.0.1", Port: state.DefaultPort,
		DefaultShell: "/bin/bash", DefaultCwd: scratch.workDir,
		DefaultCols: state.DefaultCols, DefaultRows: state.DefaultRows,
		StateRoot:       filepath.Dir(scratch.runnerDir),
		UserStateRoot:   filepath.Join(scratch.root, "user-state"),
		RunnerStateDir:  scratch.runnerDir,
		TokenPath:       filepath.Join(scratch.root, "state", "token"),
		OpenPath:        filepath.Join(scratch.root, "state", "open"),
		LaunchAgentsDir: scratch.launchAgentsDir,
		GlobalHooksPath: filepath.Join(scratch.root, "config", "hooks.json"),
		RunnerPath:      tools.goRunner,
	}
}

func startScratchGoRunner(t *testing.T, scratch scratchState, id, command string, args []string) *managedProcess {
	t.Helper()
	return startScratchGoRunnerWithEnv(t, scratch, id, command, args, nil)
}

func startScratchGoRunnerWithEnv(
	t *testing.T,
	scratch scratchState,
	id, command string,
	args []string,
	overrides map[string]string,
) *managedProcess {
	t.Helper()
	encodedArgs, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	runnerEnv := map[string]string{
		"RUNNER_ID":        id,
		"RUNNER_STATE_DIR": scratch.runnerDir,
		"RUNNER_CMD":       command,
		"RUNNER_ARGS_JSON": string(encodedArgs),
		"RUNNER_CWD":       scratch.workDir,
		"RUNNER_COLS":      "120",
		"RUNNER_ROWS":      "40",
	}
	for key, value := range overrides {
		runnerEnv[key] = value
	}
	environment := envWithoutKeys(scratch.env(runnerEnv),
		"CLAUDECODE",
		"CLAUDE_CODE_BRIDGE_SESSION_ID",
		"CLAUDE_CODE_CHILD_SESSION",
		"CLAUDE_CODE_ENTRYPOINT",
		"CLAUDE_CODE_EXECPATH",
		"CLAUDE_CODE_SESSION_ID",
	)
	runner := startProcess(t, "go-runner-"+id, scratch.root, environment, tools.goRunner)
	t.Cleanup(func() { runner.stop(t, false) })
	waitForRunnerState(t, runner, scratch.runnerDir, id)
	return runner
}

func writeAgedScratchPlist(t *testing.T, scratch scratchState, id string) {
	t.Helper()
	path := state.RunnerPlistPath(scratch.launchAgentsDir, id)
	if err := os.WriteFile(path, []byte("scratch-only label "+id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func exitedScratchPID(t *testing.T) int {
	t.Helper()
	command := exec.Command("/usr/bin/true")
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	pid := command.ProcessState.Pid()
	if process, err := os.FindProcess(pid); err == nil && process.Signal(syscall.Signal(0)) == nil {
		t.Fatalf("scratch process %d unexpectedly remained alive", pid)
	}
	return pid
}

func writeDeadScratchMetadata(t *testing.T, scratch scratchState, id string, pid int) {
	t.Helper()
	path := filepath.Join(scratch.runnerDir, id+".json")
	if err := state.WriteMetadata(path, state.Metadata{
		ID: id, Cmd: "/bin/bash", Args: []string{"-i"}, Cwd: scratch.workDir,
		Cols: 120, Rows: 40, CreatedAt: time.Now().Add(-time.Minute).UnixMilli(), PID: pid,
		SockPath: filepath.Join(scratch.runnerDir, id+".sock"),
	}); err != nil {
		t.Fatal(err)
	}
}

func assertScratchArtifactsPresent(t *testing.T, scratch scratchState, ids []string) {
	t.Helper()
	for _, id := range ids {
		for _, path := range []string{
			filepath.Join(scratch.runnerDir, id+".json"),
			state.RunnerPlistPath(scratch.launchAgentsDir, id),
		} {
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("guarded pass mutated %s: %v", path, err)
			}
		}
	}
}

func cleanupScratchClaudeProjects(t *testing.T, realHome, cwd string) {
	t.Helper()
	projectsRoot := filepath.Join(realHome, ".claude", "projects")
	for _, path := range watch.ClaudeProjectDirsUnder(projectsRoot, cwd) {
		relative, err := filepath.Rel(projectsRoot, path)
		if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			t.Fatalf("refusing unsafe Claude scratch cleanup path %s", path)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Claude scratch project path already exists: %s (%v)", path, err)
		}
		cleanupPath := path
		t.Cleanup(func() {
			// The runner sends its PTY child SIGHUP and exits immediately. Claude
			// may finish one last JSONL write just after that wrapper exits, so
			// keep removing only this exact scratch directory through the short
			// child-shutdown window instead of allowing a late recreation.
			for attempt := 0; attempt < 20; attempt++ {
				if err := os.RemoveAll(cleanupPath); err != nil {
					t.Errorf("remove scratch Claude project %s: %v", cleanupPath, err)
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
			if err := os.RemoveAll(cleanupPath); err != nil {
				t.Errorf("final remove scratch Claude project %s: %v", cleanupPath, err)
			} else if _, err := os.Stat(cleanupPath); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("scratch Claude project was recreated after cleanup: %s (%v)", cleanupPath, err)
			}
		})
	}
}

func waitForClaudeJSONL(
	t *testing.T,
	realHome, cwd, id, baseURL string,
	runner *managedProcess,
) string {
	t.Helper()
	projectsRoot := filepath.Join(realHome, ".claude", "projects")
	paths := make([]string, 0, 2)
	for _, dir := range watch.ClaudeProjectDirsUnder(projectsRoot, cwd) {
		paths = append(paths, filepath.Join(dir, id+".jsonl"))
	}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		for _, path := range paths {
			if info, err := os.Stat(path); err == nil && info.Size() > 0 {
				return path
			}
			entries, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.jsonl"))
			if len(entries) == 1 {
				if info, err := os.Stat(entries[0]); err == nil && info.Size() > 0 {
					return entries[0]
				}
			}
		}
		if !runner.alive() {
			t.Fatalf("Claude runner exited before JSONL appeared: %v\n%s", runner.waitErr, readLog(runner.logPath))
		}
		time.Sleep(100 * time.Millisecond)
	}
	snapshot := ""
	if response, err := httpClient().Get(baseURL + "/api/sessions/" + id + "/snapshot"); err == nil {
		encoded, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		snapshot = string(encoded)
	}
	t.Fatalf("Claude JSONL did not appear at %v; snapshot=%q\n%s", paths, snapshot, readLog(runner.logPath))
	return ""
}

func waitForClaudeSubmission(t *testing.T, baseURL, id, marker string) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	lastSnapshot := ""
	for time.Now().Before(deadline) {
		if events, err := httpClient().Get(baseURL + "/api/sessions/" + id + "/events?tail=20"); err == nil {
			encoded, _ := io.ReadAll(events.Body)
			_ = events.Body.Close()
			if strings.Contains(string(encoded), marker) {
				return
			}
		}
		snapshot, err := httpClient().Get(baseURL + "/api/sessions/" + id + "/snapshot")
		if err == nil {
			encoded, _ := io.ReadAll(snapshot.Body)
			_ = snapshot.Body.Close()
			lastSnapshot = string(encoded)
			lines := strings.Split(lastSnapshot, "\n")
			if len(lines) > 8 {
				lines = lines[len(lines)-8:]
			}
			composerHasMarker := false
			for _, line := range lines {
				if strings.Contains(line, "❯") && strings.Contains(line, marker) {
					composerHasMarker = true
					break
				}
			}
			if composerHasMarker {
				postSessionInput(t, baseURL, id, "\r")
			}
		}
		time.Sleep(350 * time.Millisecond)
	}
	t.Fatalf("Claude did not persist submitted marker %s; snapshot=%q", marker, lastSnapshot)
}

func postSessionInput(t *testing.T, baseURL, id, data string) {
	t.Helper()
	encoded, err := json.Marshal(map[string]string{"data": data})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+"/api/sessions/"+id+"/input", strings.NewReader(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := httpClient().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("input status=%d body=%s", response.StatusCode, body)
	}
}

type managedProcess struct {
	name    string
	command *exec.Cmd
	done    chan struct{}
	logPath string
	waitErr error
	stopMu  sync.Mutex
}

func startProcess(t *testing.T, name, dir string, env []string, argv ...string) *managedProcess {
	t.Helper()
	if len(argv) == 0 {
		t.Fatal("startProcess requires argv")
	}
	logPath := filepath.Join(dir, name+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = dir
	command.Env = env
	command.Stdout = logFile
	command.Stderr = logFile
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("start %s: %v", name, err)
	}
	process := &managedProcess{name: name, command: command, done: make(chan struct{}), logPath: logPath}
	go func() {
		process.waitErr = command.Wait()
		_ = logFile.Close()
		close(process.done)
	}()
	return process
}

func (p *managedProcess) stop(t *testing.T, requireClean bool) {
	t.Helper()
	p.stopMu.Lock()
	defer p.stopMu.Unlock()
	select {
	case <-p.done:
	default:
		_ = p.command.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			_ = p.command.Process.Kill()
			<-p.done
		}
	}
	if requireClean && p.waitErr != nil {
		t.Fatalf("%s did not stop cleanly: %v\n%s", p.name, p.waitErr, readLog(p.logPath))
	}
}

func (p *managedProcess) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return p.command.Process.Signal(syscall.Signal(0)) == nil
	}
}

func waitForRunnerState(t *testing.T, runner *managedProcess, dir, id string) {
	t.Helper()
	socketPath := filepath.Join(dir, id+".sock")
	metadataPath := filepath.Join(dir, id+".json")
	waitFor(t, 15*time.Second, func() (bool, string) {
		if !runner.alive() {
			t.Fatalf("%s exited before creating state: %v\n%s", runner.name, runner.waitErr, readLog(runner.logPath))
		}
		if info, err := os.Stat(socketPath); err != nil || info.Mode()&os.ModeSocket == 0 {
			return false, fmt.Sprintf("socket not ready: %v", err)
		}
		encoded, err := os.ReadFile(metadataPath)
		if err != nil {
			return false, fmt.Sprintf("metadata not ready: %v", err)
		}
		var metadata struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(encoded, &metadata); err != nil || metadata.ID != id {
			return false, fmt.Sprintf("metadata invalid: id=%q err=%v", metadata.ID, err)
		}
		return true, ""
	})
}

func waitForSession(t *testing.T, baseURL, id string) state.SessionInfo {
	t.Helper()
	found := waitForSessionMetadata(t, baseURL, id)
	if found.Cmd != "/bin/bash" || len(found.Args) != 1 || found.Args[0] != "-i" || found.PID <= 0 {
		t.Fatalf("discovered session metadata mismatch: %#v", found)
	}
	return found
}

func waitForSessionMetadata(t *testing.T, baseURL, id string) state.SessionInfo {
	t.Helper()
	var found state.SessionInfo
	waitFor(t, 20*time.Second, func() (bool, string) {
		response, err := httpClient().Get(baseURL + "/api/sessions")
		if err != nil {
			return false, err.Error()
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			return false, err.Error()
		}
		if response.StatusCode != http.StatusOK {
			return false, fmt.Sprintf("status=%d body=%s", response.StatusCode, body)
		}
		var envelope struct {
			Sessions []state.SessionInfo `json:"sessions"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			return false, err.Error()
		}
		for _, session := range envelope.Sessions {
			if session.ID == id && !session.Exited {
				found = session
				return true, ""
			}
		}
		return false, fmt.Sprintf("session %s absent from %s", id, body)
	})
	return found
}

func runSessions(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	output, err := runSessionsCommand(env, dir, args...)
	if err != nil {
		t.Fatalf("sessions %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return output
}

func runSessionsCommand(env []string, dir string, args ...string) (string, error) {
	command := exec.Command(tools.goSessions, args...)
	command.Dir = dir
	command.Env = env
	output, err := command.CombinedOutput()
	return string(output), err
}

func waitForSessionsLast(t *testing.T, env []string, dir, id, marker string) string {
	t.Helper()
	var output string
	waitFor(t, 120*time.Second, func() (bool, string) {
		var err error
		output, err = runSessionsCommand(env, dir, "--json", "last", id, "--role", "assistant")
		if err != nil {
			return false, fmt.Sprintf("sessions last: %v output=%s", err, output)
		}
		if strings.Contains(output, marker) {
			return true, ""
		}
		return false, fmt.Sprintf("assistant marker %s absent from %s", marker, output)
	})
	return output
}

func roundTrip(t *testing.T, baseURL, id, prefix string) string {
	t.Helper()
	prefix = strings.ToUpper(prefix)
	command := fmt.Sprintf("echo %s_$RANDOM\r", prefix)
	encoded, err := json.Marshal(map[string]string{"data": command})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+"/api/sessions/"+id+"/input", strings.NewReader(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := httpClient().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("input status=%d body=%s", response.StatusCode, body)
	}

	pattern := regexp.MustCompile(regexp.QuoteMeta(prefix) + `_[0-9]+`)
	var marker string
	waitFor(t, 15*time.Second, func() (bool, string) {
		snapshot, err := httpClient().Get(baseURL + "/api/sessions/" + id + "/snapshot")
		if err != nil {
			return false, err.Error()
		}
		defer snapshot.Body.Close()
		body, err := io.ReadAll(snapshot.Body)
		if err != nil {
			return false, err.Error()
		}
		if snapshot.StatusCode != http.StatusOK {
			return false, fmt.Sprintf("status=%d body=%s", snapshot.StatusCode, body)
		}
		if match := pattern.Find(body); match != nil {
			marker = string(match)
			return true, ""
		}
		return false, fmt.Sprintf("expanded marker absent from snapshot: %q", body)
	})
	return marker
}

func assertRunnerSurvived(t *testing.T, runner *managedProcess, socketPath string) {
	t.Helper()
	if !runner.alive() {
		t.Fatalf("runner exited with daemon: %v\n%s", runner.waitErr, readLog(runner.logPath))
	}
	if info, err := os.Stat(socketPath); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("runner socket did not survive daemon shutdown: %v", err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, probe func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := "condition not ready"
	for time.Now().Before(deadline) {
		if ok, detail := probe(); ok {
			return
		} else if detail != "" {
			last = detail
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout after %s: %s", timeout, last)
}

func unusedPort(t *testing.T) int {
	t.Helper()
	for {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		_ = listener.Close()
		if port != state.DefaultPort {
			return port
		}
	}
}

func httpClient() *http.Client {
	return &http.Client{Timeout: 2 * time.Second}
}

func newUUID(t *testing.T) string {
	t.Helper()
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		t.Fatal(err)
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(bytes)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}

func randomHex(t *testing.T, byteCount int) string {
	t.Helper()
	bytes := make([]byte, byteCount)
	if _, err := rand.Read(bytes); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(bytes)
}

func runBuild(dir string, env []string, argv ...string) error {
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = dir
	command.Env = env
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w\n%s", strings.Join(argv, " "), err, output)
	}
	return nil
}

func envWith(current []string, overrides map[string]string) []string {
	result := make([]string, 0, len(current)+len(overrides))
	for _, entry := range current {
		key, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		if _, overridden := overrides[key]; !overridden {
			result = append(result, entry)
		}
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
}

func envWithoutKeys(current []string, keys ...string) []string {
	removed := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		removed[key] = struct{}{}
	}
	result := make([]string, 0, len(current))
	for _, entry := range current {
		key, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		if _, drop := removed[key]; !drop {
			result = append(result, entry)
		}
	}
	return result
}

func findRepoRoot() (string, error) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("resolve test source path")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "runtime", "testdata", "node-runtime", "src", "runner.ts")); err != nil {
		return "", fmt.Errorf("resolve repository root %s: %w", root, err)
	}
	return root, nil
}

func readLog(path string) string {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("read %s: %v", path, err)
	}
	return string(encoded)
}

func requireDarwin(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("the normative node runner uses node-pty and is supported on darwin")
	}
}
