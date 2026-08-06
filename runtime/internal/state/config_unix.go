//go:build !windows

package state

import (
	"fmt"
	"os"
	"path/filepath"
)

func defaultStateRoot(home string) string {
	return filepath.Join(home, ".local", "state", "sessions")
}

func userConfigRoot(home string) string {
	return filepath.Join(home, ".config", "sessions")
}

func serviceDefinitionsDir(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents")
}

func defaultShell() string {
	return "/bin/bash"
}

func runnerBinaryNames(goos, goarch string) []string {
	return []string{
		"sessions-runner",
		fmt.Sprintf("sessions-runner-%s-%s", goos, goarch),
	}
}

func isExecutableFile(path string) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func executableCandidates(path string) []string {
	return []string{path}
}
