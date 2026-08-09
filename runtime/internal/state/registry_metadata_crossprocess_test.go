package state

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file is a regression test for a cross-process lost update on one
// session's metadata document.
//
// Two different PROCESSES perform a read-modify-write on
// <RunnerStateDir>/<id>.json with nothing serialising them:
//
//   - the runner, launched by launchd and therefore not a child of the daemon,
//     through state.WriteRunnerMetadata (metadata_merge.go:21-23), which reads
//     the document at metadata_merge.go:63-73 and writes it back at
//     metadata_merge.go:22 via WriteMetadata (paths.go:146).
//   - the daemon, through Registry.UpdateTags (registry.go:463), which reads
//     the document at registry.go:473 and writes it back at registry.go:485.
//
// Each individual write is atomic (paths.go writes a temp file and renames it),
// and MergeRunnerMetadata stops the runner from erasing daemon-owned fields it
// read. Neither alone is a lock. The shared sidecar lock must cover the complete
// read-modify-write or whichever process renames last writes back the document
// it read and the other process's update is gone.
//
// The tests below drive both sides through the real production functions. They
// model two processes rather than two goroutines on purpose: a lost update
// between goroutines could be closed by an in-process mutex, which would prove
// nothing about a runner that launchd started in a separate address space.
// The runner side runs in a re-invocation of this test binary
// (TestCrossProcessRunnerMetadataHelper) so that no in-memory state whatsoever
// is shared with the daemon side.

const (
	crossProcessHelperEnv   = "SESSIONS_TEST_METADATA_HELPER"
	crossProcessPathEnv     = "SESSIONS_TEST_METADATA_PATH"
	crossProcessDonePathEnv = "SESSIONS_TEST_METADATA_DONE_PATH"

	crossProcessSessionID = "11111111-2222-4333-8444-555555555555"
	// crossProcessClaudeID is the provider conversation id the runner discovers
	// at runtime and persists. The daemon cannot know it at create time.
	crossProcessClaudeID = "claude-conversation-77e1f6b0"

	// crossProcessPaddingArgs pads the session's argv so that decoding and
	// re-encoding the document costs hundreds of milliseconds instead of
	// microseconds. This is the deterministic window-widener: it does not
	// create the defect, it only makes the existing unsynchronised window wide
	// enough that the interleaving is reached every run instead of once in
	// however many thousand. It also makes the outcome deterministic in a
	// second way: the daemon re-serialises everything it read, so its write is
	// slow, while the runner rebuilds the document from its own launch
	// configuration, so its write is fast. The daemon therefore renames last.
	crossProcessPaddingArgs = 800_000

	crossProcessBudget = 90 * time.Second
)

// TestCrossProcessRunnerMetadataHelper is not a test. It is the runner process:
// the cross-process tests below re-invoke this test binary with
// SESSIONS_TEST_METADATA_HELPER set, and this entry point performs exactly one
// real state.WriteRunnerMetadata call, the same call the PTY runner makes at
// cmd/sessions-runner/main.go:360 and the structured runners make at
// cmd/sessions-runner/claude_p.go:189 and cmd/sessions-runner/codex_app.go:234.
func TestCrossProcessRunnerMetadataHelper(t *testing.T) {
	if os.Getenv(crossProcessHelperEnv) != "1" {
		t.Skip("helper process entry point for the cross-process metadata tests")
	}
	// This process must never outlive the test that spawned it, even if the
	// parent dies without closing the release pipe.
	watchdog := time.AfterFunc(crossProcessBudget, func() {
		fmt.Fprintln(os.Stderr, "runner helper: watchdog expired without a release byte")
		os.Exit(9)
	})
	defer watchdog.Stop()

	path := os.Getenv(crossProcessPathEnv)
	donePath := os.Getenv(crossProcessDonePathEnv)

	// The synchronisation point both processes observe: one byte on stdin,
	// written by the parent immediately before the daemon side starts its own
	// read-modify-write. No sleeping, no hoping.
	if _, err := io.ReadFull(os.Stdin, make([]byte, 1)); err != nil {
		fmt.Fprintf(os.Stderr, "runner helper: read release byte: %v\n", err)
		os.Exit(4)
	}

	if err := WriteRunnerMetadata(path, runnerOwnedMetadata(os.Getpid())); err != nil {
		fmt.Fprintf(os.Stderr, "runner helper: WriteRunnerMetadata: %v\n", err)
		os.Exit(5)
	}
	if err := os.WriteFile(donePath, []byte(strconv.FormatInt(time.Now().UnixNano(), 10)), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "runner helper: record completion: %v\n", err)
		os.Exit(6)
	}
}

// TestRunnerMetadataWriteAndDaemonTagEditLoseAnUpdateAcrossProcesses is the
// regression test. It fails on a tree with no advisory lock around the two
// read-modify-writes.
func TestRunnerMetadataWriteAndDaemonTagEditLoseAnUpdateAcrossProcesses(t *testing.T) {
	fixture := newCrossProcessMetadataFixture(t)
	runner := fixture.startRunnerProcess(t)

	// Interleave. The release byte is written from inside the same goroutine
	// that then calls the daemon's read-modify-write, so the runner process is
	// released microseconds before Registry.UpdateTags opens the document,
	// while each side needs hundreds of milliseconds to complete. The two
	// windows overlap every run.
	var (
		waitGroup  sync.WaitGroup
		daemonErr  error
		daemonTags map[string]string
		daemonDone time.Time
	)
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()
		if err := runner.release(); err != nil {
			daemonErr = err
			return
		}
		daemonTags, daemonErr = fixture.registry.UpdateTags(crossProcessSessionID, map[string]string{"team": "native"})
		daemonDone = time.Now()
	}()

	runner.waitForExit(t)
	waitForGroup(t, &waitGroup, "daemon UpdateTags")
	if daemonErr != nil {
		t.Fatalf("daemon UpdateTags failed: %v", daemonErr)
	}
	if daemonTags["team"] != "native" {
		t.Fatalf("daemon UpdateTags returned %#v, want the tag it acknowledged to the user", daemonTags)
	}

	fixture.assertNothingWasLost(t, runner, "the runner's metadata write and the user's tag edit overlapped", daemonDone)
}

// TestRunnerMetadataWriteAndDaemonTagEditSurviveWhenSerialised is the control.
// It runs exactly the same two processes and exactly the same two production
// read-modify-writes, but without overlapping them: the runner process is
// released and fully reaped before the daemon starts. It passes on the current
// tree, which is how you can tell the test above detects the interleaving and
// not something else. It is also the outcome a correct advisory lock produces
// for the overlapping case.
func TestRunnerMetadataWriteAndDaemonTagEditSurviveWhenSerialised(t *testing.T) {
	fixture := newCrossProcessMetadataFixture(t)
	runner := fixture.startRunnerProcess(t)

	if err := runner.release(); err != nil {
		t.Fatalf("release runner process: %v", err)
	}
	runner.waitForExit(t)

	daemonTags, err := fixture.registry.UpdateTags(crossProcessSessionID, map[string]string{"team": "native"})
	if err != nil {
		t.Fatalf("daemon UpdateTags failed: %v", err)
	}
	if daemonTags["team"] != "native" {
		t.Fatalf("daemon UpdateTags returned %#v, want the tag it acknowledged to the user", daemonTags)
	}

	fixture.assertNothingWasLost(t, runner, "the runner's metadata write completed before the user's tag edit began", time.Now())
}

type crossProcessMetadataFixture struct {
	dir      string
	path     string
	donePath string
	registry *Registry
}

func newCrossProcessMetadataFixture(t *testing.T) *crossProcessMetadataFixture {
	t.Helper()
	root := t.TempDir()
	runnerDir := filepath.Join(root, "runners")
	if err := os.MkdirAll(runnerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := &crossProcessMetadataFixture{
		dir:      runnerDir,
		path:     filepath.Join(runnerDir, crossProcessSessionID+".json"),
		donePath: filepath.Join(root, "runner-done"),
		registry: NewRegistry(Config{RunnerStateDir: runnerDir}, nil),
	}
	// The document as the daemon wrote it at create time (registry.go:889): no
	// tags yet, no provider conversation id yet, and no runner pid yet.
	if err := WriteMetadata(fixture.path, daemonCreatedMetadata()); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *crossProcessMetadataFixture) startRunnerProcess(t *testing.T) *crossProcessRunner {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(
		executable,
		"-test.run=^TestCrossProcessRunnerMetadataHelper$",
		"-test.count=1",
		"-test.timeout="+crossProcessBudget.String(),
	)
	command.Env = append(os.Environ(),
		crossProcessHelperEnv+"=1",
		crossProcessPathEnv+"="+f.path,
		crossProcessDonePathEnv+"="+f.donePath,
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output := &lockedBuffer{}
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	runner := &crossProcessRunner{command: command, stdin: stdin, output: output, exit: make(chan error, 1)}
	go func() { runner.exit <- command.Wait() }()
	t.Cleanup(func() {
		_ = stdin.Close()
		if runner.command.ProcessState == nil && runner.command.Process != nil {
			_ = runner.command.Process.Kill()
		}
	})
	return runner
}

func (f *crossProcessMetadataFixture) assertNothingWasLost(
	t *testing.T,
	runner *crossProcessRunner,
	interleaving string,
	daemonDone time.Time,
) {
	t.Helper()
	final, err := ReadRunnerMetadata(f.path)
	if err != nil {
		t.Fatalf("read the metadata document both processes wrote: %v", err)
	}
	var lost []string
	if final.Tags["team"] != "native" {
		lost = append(lost, "the tag the user set in the app (team=native) was discarded by the runner's "+
			"metadata write, so the session comes back untagged after the next daemon restart")
	}
	if final.Info.ClaudeSessionID != crossProcessClaudeID || final.Info.PID != runner.pid() {
		lost = append(lost, "the live pid and the Claude conversation id the runner published were discarded "+
			"by the daemon's tag edit, so after a daemon restart discovery re-reads the pre-launch document and "+
			"the running session comes back unresumable")
	}
	if len(lost) == 0 {
		return
	}
	t.Fatalf(
		"lost update on %s\n\n%s\n\nwhat was lost:\n  - %s\n\n"+
			"both processes did an unsynchronised read-modify-write on the same document "+
			"(runner: state.WriteRunnerMetadata, metadata_merge.go:21; daemon: Registry.UpdateTags, registry.go:463); "+
			"whichever renamed last wrote back the document it had already read\n\n"+
			"runner process pid %d finished at %s, daemon finished at %s\n"+
			"final document: pid=%d claudeSessionId=%q tags=%v\nrunner process output:\n%s",
		f.path,
		interleaving,
		strings.Join(lost, "\n  - "),
		runner.pid(), f.completedAt(), daemonDone.Format(time.RFC3339Nano),
		final.Info.PID, final.Info.ClaudeSessionID, final.Tags,
		runner.output.String(),
	)
}

func (f *crossProcessMetadataFixture) completedAt() string {
	raw, err := os.ReadFile(f.donePath)
	if err != nil {
		return "never"
	}
	nanos, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return "unknown"
	}
	return time.Unix(0, nanos).Format(time.RFC3339Nano)
}

type crossProcessRunner struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	output  *lockedBuffer
	exit    chan error
}

func (r *crossProcessRunner) release() error {
	if _, err := r.stdin.Write([]byte{'g'}); err != nil {
		return fmt.Errorf("release runner process: %w", err)
	}
	return nil
}

func (r *crossProcessRunner) pid() int { return r.command.Process.Pid }

// waitForExit never blocks forever: every wait in this file is a select with a
// deadline, because a hanging test reports nothing.
func (r *crossProcessRunner) waitForExit(t *testing.T) {
	t.Helper()
	select {
	case err := <-r.exit:
		if err != nil {
			t.Fatalf("runner process failed: %v\noutput:\n%s", err, r.output.String())
		}
	case <-time.After(crossProcessBudget):
		_ = r.command.Process.Kill()
		t.Fatalf("runner process did not finish within %s\noutput:\n%s", crossProcessBudget, r.output.String())
	}
}

func waitForGroup(t *testing.T, group *sync.WaitGroup, what string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(crossProcessBudget):
		t.Fatalf("%s did not finish within %s", what, crossProcessBudget)
	}
}

// daemonCreatedMetadata is the document Registry.Create persists before the
// runner is launched.
func daemonCreatedMetadata() Metadata {
	return Metadata{
		ID: crossProcessSessionID, Name: "hardening sweep", Kind: KindClaudeStructured,
		Cmd: "claude", Args: paddedLaunchArgs(), Cwd: "/tmp",
		Cols: 300, Rows: 50, CreatedAt: 1_700_000_000_000,
		SockPath: filepath.Join("/tmp", crossProcessSessionID+".sock"),
	}
}

// runnerOwnedMetadata is the document the runner rebuilds from its launch
// configuration, carrying the two facts only the runner knows.
func runnerOwnedMetadata(pid int) Metadata {
	return Metadata{
		ID: crossProcessSessionID, Name: "hardening sweep", Kind: KindClaudeStructured,
		Cmd: "claude", Args: []string{"--session-id", crossProcessSessionID}, Cwd: "/tmp",
		Cols: 300, Rows: 50, CreatedAt: 1_700_000_000_000,
		PID: pid, SockPath: filepath.Join("/tmp", crossProcessSessionID+".sock"),
		ClaudeSessionID: crossProcessClaudeID,
	}
}

func paddedLaunchArgs() []string {
	args := make([]string, 0, crossProcessPaddingArgs+2)
	args = append(args, "--session-id", crossProcessSessionID)
	for index := 0; index < crossProcessPaddingArgs; index++ {
		args = append(args, fmt.Sprintf("--metadata-window-padding-%09d", index))
	}
	return args
}

type lockedBuffer struct {
	mu    sync.Mutex
	bytes []byte
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bytes = append(b.bytes, p...)
	return len(p), nil
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.bytes)
}
