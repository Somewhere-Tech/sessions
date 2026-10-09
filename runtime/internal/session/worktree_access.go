package session

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func worktreeSourceError(cwd string, cause error) error {
	const remedy = "--worktree needs --cwd (or $PWD) inside a Git repository that this process can read; cd into a repository or pass --cwd and retry"
	if cause == nil {
		return errors.New(remedy)
	}
	return fmt.Errorf("%s (%s): %w", remedy, cwd, cause)
}

// Diagnose only a failed Git command, in the daemon's own access scope. This
// does not claim that a provider sandbox or another process has the same access.
// A worktree's .git pointer and commondir can cross volumes even when cwd works.
func explainGitMetadataAccess(cwd string, cause error) error {
	path, err := findGitMetadata(cwd)
	if err == nil && path != "" {
		err = inspectGitMetadata(path)
	}
	if err == nil {
		return cause
	}
	return fmt.Errorf("%w; cannot read Git metadata: %v. Check that the source repository/drive is present and this process has access; on macOS, review Sessions in System Settings > Privacy & Security > Files and Folders. Provider sandbox approvals are separate. No files or permissions were changed", cause, err)
}

func findGitMetadata(cwd string) (string, error) {
	directory, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	for {
		path := filepath.Join(directory, ".git")
		info, err := os.Stat(path)
		if err == nil {
			if info.IsDir() {
				return path, nil
			}
			value, err := readGitPointer(path)
			if err != nil {
				return "", err
			}
			if !strings.HasPrefix(value, "gitdir: ") {
				return "", nil // Leave malformed-pointer diagnosis to Git.
			}
			return resolveGitPointer(directory, strings.TrimSpace(strings.TrimPrefix(value, "gitdir: "))), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", nil
		}
		directory = parent
	}
}

func inspectGitMetadata(directory string) error {
	if _, err := readGitPointer(filepath.Join(directory, "HEAD")); err != nil {
		return err
	}
	common, err := readGitPointer(filepath.Join(directory, "commondir"))
	if errors.Is(err, os.ErrNotExist) {
		return nil // An ordinary checkout has no commondir indirection.
	}
	if err != nil {
		return err
	}
	_, err = readGitPointer(filepath.Join(resolveGitPointer(directory, common), "HEAD"))
	return err
}

func resolveGitPointer(directory, value string) string {
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Join(directory, value)
}

func readGitPointer(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return "", fmt.Errorf("Git metadata %s is not a regular file of at most 4096 bytes", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("Git metadata %s is not a regular file", path)
	}
	value, err := io.ReadAll(io.LimitReader(file, 4096))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return strings.TrimSpace(string(value)), nil
}
