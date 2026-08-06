//go:build windows

package state

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func sessionsAppRoot(home string) string {
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" {
		localAppData = filepath.Join(home, "AppData", "Local")
	}
	return filepath.Join(localAppData, "Sessions")
}

func defaultStateRoot(home string) string {
	return filepath.Join(sessionsAppRoot(home), "state")
}

// userConfigRoot is the Windows adapter for the Unix ~/.config/sessions tree.
// It sits beside the state root rather than under it so a state reset cannot
// discard configuration or the backup key.
func userConfigRoot(home string) string {
	return filepath.Join(sessionsAppRoot(home), "config")
}

func serviceDefinitionsDir(home string) string {
	return filepath.Join(defaultStateRoot(home), "supervision")
}

func defaultShell() string {
	if shell := os.Getenv("COMSPEC"); shell != "" {
		return shell
	}
	return "cmd.exe"
}

func runnerBinaryNames(goos, goarch string) []string {
	return []string{
		"sessions-runner.exe",
		fmt.Sprintf("sessions-runner-%s-%s.exe", goos, goarch),
	}
}

func isExecutableFile(path string) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func executableCandidates(path string) []string {
	if filepath.Ext(path) != "" {
		return []string{path}
	}
	extensions := filepath.SplitList(os.Getenv("PATHEXT"))
	if len(extensions) == 0 {
		extensions = []string{".COM", ".EXE", ".BAT", ".CMD"}
	}
	result := []string{path}
	for _, extension := range extensions {
		if extension = strings.TrimSpace(extension); extension != "" {
			result = append(result, path+strings.ToLower(extension), path+strings.ToUpper(extension))
		}
	}
	return result
}
