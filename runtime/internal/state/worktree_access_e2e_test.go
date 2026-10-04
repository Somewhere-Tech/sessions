//go:build !windows

package state_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Verify the real runner, not just a daemon-side Git probe. This tests ordinary
// filesystem access across a worktree pointer; it cannot simulate macOS TCC or
// a provider's own sandbox on an external drive.
func TestRunnerCanUseSharedGitMetadataThroughSymlinkedSource(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the actual daemon, CLI, and runner")
	}
	root, err := os.MkdirTemp("/tmp", "s-git-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	fleet := buildSessionsBinaries(t, root)
	environment := isolatedDaemonEnvironment(t, root, fleet)
	source := filepath.Join(root, "source-on-other-drive")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	git := func(cwd string, args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", cwd}, args...)...)
		command.Env = environment.values
		value, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("Git %v: %v\n%s", args, err, value)
		}
		return strings.TrimSpace(string(value))
	}
	git(source, "init", "-b", "main")
	git(source, "config", "user.name", "Sessions isolated test")
	git(source, "config", "user.email", "test@example.invalid")
	git(source, "commit", "--allow-empty", "-m", "initial")
	initial := git(source, "rev-parse", "HEAD")
	original := filepath.Join(root, "original-source")
	if err := os.Symlink(source, original); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(root, "working-files", "lane")
	git(original, "worktree", "add", "-b", "runner-smoke", worktree)
	daemon := startDaemon(t, fleet, environment)
	t.Cleanup(func() { _ = syscall.Kill(-daemon.Process.Pid, syscall.SIGKILL); _ = daemon.Wait() })
	waitForDaemon(t, environment.port)
	id := strings.TrimSpace(runCLI(t, fleet.cli, environment.values, "run", "--name", "git-scope-test", "--cwd", worktree,
		"--", "git", "commit", "--allow-empty", "-m", "runner shared metadata proof"))
	completion := runCLI(t, fleet.cli, environment.values, "--json", "wait", id, "--timeout", "30s")
	var receipt struct {
		OK   bool `json:"ok"`
		Lane struct {
			ExitCode int `json:"exit_code"`
		} `json:"lane"`
	}
	if err := json.Unmarshal([]byte(completion), &receipt); err != nil || !receipt.OK || receipt.Lane.ExitCode != 0 {
		t.Fatalf("runner Git commit failed: %s, %v", completion, err)
	}
	if git(source, "rev-parse", "HEAD") != initial || git(worktree, "rev-parse", "HEAD") == initial {
		t.Fatal("runner failed to update its own branch, or changed the source branch")
	}
}
