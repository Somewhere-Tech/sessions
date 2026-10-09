package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitAccessDiagnosticFollowsWorktreeAndCommonDirectory(t *testing.T) {
	root := t.TempDir()
	repo := initWorktreeTestRepo(t, root, "source")
	lane := filepath.Join(root, "separate-drive", "lane")
	gitTest(t, repo, "worktree", "add", "-b", "lane", lane)
	if err := os.MkdirAll(filepath.Join(lane, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	metadata, err := findGitMetadata(filepath.Join(lane, "nested"))
	if err != nil || metadata != filepath.Join(repo, ".git", "worktrees", "lane") {
		t.Fatalf("worktree metadata = %q, %v", metadata, err)
	}
	if err := inspectGitMetadata(metadata); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("unrelated Git failure")
	if got := explainGitMetadataAccess(lane, cause); got != cause {
		t.Fatalf("readable Git metadata masked real error: %v", got)
	}
	commonHead := filepath.Join(repo, ".git", "HEAD")
	if err := os.Rename(commonHead, commonHead+".saved"); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(commonHead+".saved", commonHead)
	got := explainGitMetadataAccess(lane, cause)
	if !errors.Is(got, cause) || !strings.Contains(got.Error(), commonHead) || !strings.Contains(got.Error(), "No files or permissions were changed") {
		t.Fatalf("lost common-dir diagnosis or original error: %v", got)
	}
}

func TestGitWorktreeAccessFailurePreservesPathAndOriginalError(t *testing.T) {
	root := t.TempDir()
	lane := filepath.Join(root, "lane")
	if err := os.MkdirAll(lane, 0o700); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "unmounted-source", "worktrees", "lane")
	pointer := "gitdir: " + missing + "\n"
	if err := os.WriteFile(filepath.Join(lane, ".git"), []byte(pointer), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := createGitWorktree(context.Background(), lane, "new", "")
	if err == nil || !strings.Contains(err.Error(), "not a git repository") || !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "cannot read Git metadata") {
		t.Fatalf("Git failure was hidden: %v", err)
	}
	value, readErr := os.ReadFile(filepath.Join(lane, ".git"))
	if readErr != nil || string(value) != pointer {
		t.Fatalf("diagnosis changed worktree pointer: %q, %v", value, readErr)
	}
}

func TestGitAccessDiagnosticReportsDeniedMetadata(t *testing.T) {
	root := t.TempDir()
	repo := initWorktreeTestRepo(t, root, "source")
	head := filepath.Join(repo, ".git", "HEAD")
	if err := os.Chmod(head, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(head, 0o600)
	if _, err := os.ReadFile(head); err == nil {
		t.Skip("filesystem/user does not enforce this read denial")
	}
	cause := errors.New("Git failed")
	got := explainGitMetadataAccess(repo, cause)
	if !errors.Is(got, cause) || !strings.Contains(got.Error(), head) || !strings.Contains(got.Error(), "permission denied") {
		t.Fatalf("missing permission diagnosis: %v", got)
	}
}

func TestGitAccessDiagnosticAcceptsRelativePointerAndDoesNotInventAccessFailure(t *testing.T) {
	root := t.TempDir()
	initWorktreeTestRepo(t, root, "source")
	repo := filepath.Join(root, "source")
	lane := filepath.Join(root, "lane")
	if err := os.MkdirAll(lane, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lane, ".git"), []byte("gitdir: ../source/.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := findGitMetadata(lane)
	if err != nil || path != filepath.Join(repo, ".git") {
		t.Fatalf("relative pointer = %q, %v", path, err)
	}
	cause := errors.New("Git failed")
	if got := explainGitMetadataAccess(lane, cause); got != cause {
		t.Fatalf("false access diagnosis: %v", got)
	}
	if got := explainGitMetadataAccess(t.TempDir(), cause); got != cause {
		t.Fatalf("non-repository invented access failure: %v", got)
	}
}

func TestGitDiagnosticReadsOnlySmallRegularMetadata(t *testing.T) {
	root := t.TempDir()
	if _, err := readGitPointer(root); err == nil {
		t.Fatal("accepted a directory as Git metadata")
	}
	oversized := filepath.Join(root, "pointer")
	if err := os.WriteFile(oversized, []byte(strings.Repeat("x", 4097)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readGitPointer(oversized); err == nil {
		t.Fatal("silently truncated an oversized metadata pointer")
	}
}
