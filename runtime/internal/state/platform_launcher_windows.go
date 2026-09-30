//go:build windows

package state

import "github.com/somewhere-tech/sessions/runtime/internal/proto"

// NewPlatformLauncher hands Windows the launcher that knows how a Windows
// runner is identified and ended: a pid plus its creation time, and a Job
// Object that owns the provider. SESSIONS_LAUNCHER=detached selects the
// portable launcher instead, which starts the same process with the same
// creation flags but identifies it only as one this daemon started.
func NewPlatformLauncher(config Config) proto.RunnerLauncher {
	if DetachedLauncherRequested() {
		return NewDetachedLauncher(config)
	}
	return NewWindowsLauncher(config)
}
