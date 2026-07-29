package usage

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// NewLocalService applies the daemon's local state and provider-profile roots
// to one usage service. StateRoot deliberately wins so scratch daemons cannot
// read or write the installed daemon's usage ledger.
func NewLocalService(config state.Config) *Service {
	usageRoot := config.StateRoot
	if usageRoot == "" {
		usageRoot = config.UserStateRoot
	}
	claudeRoots := []string{
		filepath.Join(config.DefaultCwd, ".claude", "projects"),
		filepath.Join(config.DefaultCwd, ".config", "claude", "projects"),
	}
	if configured := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); configured != "" {
		claudeRoots = append(claudeRoots, filepath.Join(configured, "projects"))
	}
	codexRoot := filepath.Join(config.DefaultCwd, ".codex")
	if configured := strings.TrimSpace(os.Getenv("CODEX_HOME")); configured != "" {
		codexRoot = configured
	}
	return NewService(Options{
		Path: filepath.Join(usageRoot, "usage.sqlite3"), ClaudeRoots: claudeRoots,
		CodexRoots: []string{filepath.Join(codexRoot, "sessions")}, RunnerStateDir: config.RunnerStateDir,
	})
}
