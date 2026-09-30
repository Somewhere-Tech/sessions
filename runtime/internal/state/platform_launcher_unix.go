//go:build !windows

package state

import (
	"runtime"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
)

// NewPlatformLauncher is how a machine gets the launcher that can actually
// start a session on it.
//
// macOS has launchd, which supervises a runner across a crash and a login, so
// that is what macOS uses. Every other Unix has no launchd at all: this file
// used to hand them LaunchdLauncher anyway, so on Linux every `sessions run`
// and `sessions new` failed with `launchctl bootstrap …: executable file not
// found in $PATH` while the read-only commands worked. They get the detached
// launcher, which starts the runner itself.
//
// SESSIONS_LAUNCHER=detached selects it on macOS too. That is not a preference
// knob: it is how a container with no launchctl, and the tests that prove this
// path, run the same launcher Linux runs on a host that happens to have
// launchd.
func NewPlatformLauncher(config Config) proto.RunnerLauncher {
	if runtime.GOOS == "darwin" && !DetachedLauncherRequested() {
		return NewLaunchdLauncher(config)
	}
	return NewDetachedLauncher(config)
}
