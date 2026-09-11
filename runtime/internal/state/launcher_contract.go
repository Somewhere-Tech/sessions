package state

import (
	"context"
	"os"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
)

// Launcher is everything one platform has to do to own a runner's lifetime.
//
// It exists because there used to be no such contract: the daemon held a
// *LaunchdLauncher on every Unix, so on Linux every launch failed with
// `launchctl bootstrap …: executable file not found in $PATH` while the
// read-only commands worked, and nothing in the code said that a platform
// without launchd had no way to start a session.
//
// The methods are the whole lifetime of a runner, in the order they happen:
// ProgramArguments (what will be executed), Prepare (durable state written
// before anything starts), Preflight (refuse early, with a reason), Launch,
// Attach (re-adopt a runner this daemon did not start — a restart, or another
// daemon incarnation), Wake (bring back one that deliberately stayed stopped),
// and Reap (end what a failed or finished launch left behind).
//
// Registry asks for the optional pieces through narrow interface assertions,
// so a launcher may implement fewer of them; WindowsLauncher does, having no
// plist to prepare and no launchd job to wake. The two launchers that own a
// whole platform — LaunchdLauncher on macOS, DetachedLauncher everywhere else
// — implement all of it, and the assertions below are what keeps them honest.
type Launcher interface {
	proto.RunnerLauncher
	Prepare(proto.LaunchRequest) error
	Preflight(proto.LaunchRequest) error
	Wake(context.Context, string) (proto.Runner, error)
	Reap(string) error
}

var (
	_ Launcher = (*LaunchdLauncher)(nil)
	_ Launcher = (*DetachedLauncher)(nil)
)

// LauncherEnvVar names the launcher to use instead of the platform default.
// The only value that changes anything is "detached": it is how a macOS
// container or a test runs the launcher every other platform uses, on a host
// that happens to have launchd.
const LauncherEnvVar = "SESSIONS_LAUNCHER"

// DetachedLauncherRequested reports whether the environment asked for the
// detached launcher explicitly.
func DetachedLauncherRequested() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(LauncherEnvVar)), "detached")
}
