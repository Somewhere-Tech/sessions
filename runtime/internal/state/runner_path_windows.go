//go:build windows

package state

import (
	"os"
	"path/filepath"
	"strings"
)

func platformRunnerPath(value string) string {
	parts := filepath.SplitList(value)
	home, _ := os.UserHomeDir()
	appData := os.Getenv("APPDATA")
	if appData == "" && home != "" {
		appData = filepath.Join(home, "AppData", "Roaming")
	}
	candidates := make([]string, 0, 5)
	if executable, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Dir(executable))
	}
	if appData != "" {
		candidates = append(candidates, filepath.Join(appData, "npm"))
	}
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, ".cargo", "bin"),
		)
	}
	seen := make(map[string]struct{}, len(parts)+len(candidates))
	result := make([]string, 0, len(parts)+len(candidates))
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		key := strings.ToLower(filepath.Clean(path))
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		result = append(result, path)
	}
	for _, candidate := range candidates {
		add(candidate)
	}
	for _, part := range parts {
		add(part)
	}
	return strings.Join(result, string(os.PathListSeparator))
}
