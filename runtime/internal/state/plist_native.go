package state

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Match the native shell's managed runtime location, not an arbitrary runner
// executable. Scratch and standalone launchers must not borrow the installed
// app's Login Items attribution. Association is display attribution only: macOS
// privacy still holds the runner responsible for its children, and it does not
// grant access or change provider approval/sandbox policy.
func nativeRunnerAppAssociation(config Config) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || filepath.Clean(config.LaunchAgentsDir) != filepath.Join(home, "Library", "LaunchAgents") {
		return ""
	}
	root := filepath.Join(home, "Library", "Application Support", "Sessions", "runtime")
	path := filepath.Clean(config.RunnerPath)
	if filepath.Base(path) != "sessions-runner" || !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return ""
	}
	return "tech.somewhere.sessions"
}
