package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGitFilesChangedSinceIgnoresPreexistingDirtyFiles(t *testing.T) {
	repository := t.TempDir()
	runGit(t, repository, "init")
	runGit(t, repository, "config", "user.email", "sessions@example.test")
	runGit(t, repository, "config", "user.name", "Sessions Test")
	writeTestFile(t, filepath.Join(repository, "tracked.txt"), "initial\n")
	runGit(t, repository, "add", "tracked.txt")
	runGit(t, repository, "commit", "-m", "initial")

	writeTestFile(t, filepath.Join(repository, "already-dirty.txt"), "preexisting\n")
	before := captureGitWorktreeState(repository)
	if before.root == "" {
		t.Fatal("expected Git worktree baseline")
	}
	if got := gitFilesChangedSince(repository, before); got == nil || *got != 0 {
		t.Fatalf("unchanged dirty worktree delta = %v, want 0", got)
	}

	writeTestFile(t, filepath.Join(repository, "tracked.txt"), "lane edit\n")
	writeTestFile(t, filepath.Join(repository, "created-by-lane.txt"), "new\n")
	if got := gitFilesChangedSince(repository, before); got == nil || *got != 2 {
		t.Fatalf("lane worktree delta = %v, want 2", got)
	}
}

func TestGitFilesChangedSinceCountsCommittedWorkFromNestedDirectory(t *testing.T) {
	repository := t.TempDir()
	runGit(t, repository, "init")
	runGit(t, repository, "config", "user.email", "sessions@example.test")
	runGit(t, repository, "config", "user.name", "Sessions Test")
	nested := filepath.Join(repository, "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(nested, "tracked.txt"), "initial\n")
	runGit(t, repository, "add", "nested/tracked.txt")
	runGit(t, repository, "commit", "-m", "initial")

	writeTestFile(t, filepath.Join(nested, "already-dirty.txt"), "preexisting\n")
	before := captureGitWorktreeState(nested)
	writeTestFile(t, filepath.Join(nested, "tracked.txt"), "committed lane edit\n")
	runGit(t, repository, "add", "nested/tracked.txt")
	runGit(t, repository, "commit", "-m", "lane edit")

	if got := gitFilesChangedSince(nested, before); got == nil || *got != 1 {
		t.Fatalf("committed nested worktree delta = %v, want 1", got)
	}
}

func TestGitFilesChangedSinceCountsFirstCommitInUnbornRepository(t *testing.T) {
	repository := t.TempDir()
	runGit(t, repository, "init")
	runGit(t, repository, "config", "user.email", "sessions@example.test")
	runGit(t, repository, "config", "user.name", "Sessions Test")
	before := captureGitWorktreeState(repository)

	writeTestFile(t, filepath.Join(repository, "first.txt"), "first commit\n")
	runGit(t, repository, "add", "first.txt")
	runGit(t, repository, "commit", "-m", "first")

	if got := gitFilesChangedSince(repository, before); got == nil || *got != 1 {
		t.Fatalf("first commit delta = %v, want 1", got)
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
}
