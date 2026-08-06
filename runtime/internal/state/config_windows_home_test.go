//go:build windows

package state

import (
	"path/filepath"
	"strings"
	"testing"
)

// os.UserHomeDir reads USERPROFILE on Windows, so setting it decides which home
// counts as the signed-in user's for the duration of a test.
const userProfileVariable = "USERPROFILE"

// A caller that passes a home means it: a test with a temp directory, or a
// scratch daemon started per the isolation recipe in AGENTS.md. LOCALAPPDATA
// names one specific profile, so following it for some other home aims both at
// the signed-in user's real state -- silently, because those paths still
// resolve and still work.
func TestAnExplicitHomeIsNotOverriddenByTheEnvironment(t *testing.T) {
	realProfile := filepath.Join(t.TempDir(), "RealUser")
	t.Setenv(userProfileVariable, realProfile)
	t.Setenv("LOCALAPPDATA", filepath.Join(realProfile, "AppData", "Local"))

	scratch := t.TempDir()
	for name, root := range map[string]string{
		"state":  UserStateRootFor(scratch),
		"config": UserConfigRootFor(scratch),
	} {
		if !strings.HasPrefix(root, filepath.Clean(scratch)) {
			t.Errorf("%s root for a scratch home resolved to %q, outside %q — "+
				"a test or scratch daemon would read and write the real user's state",
				name, root, scratch)
		}
		if strings.Contains(root, "RealUser") {
			t.Errorf("%s root %q reached the profile named by LOCALAPPDATA", name, root)
		}
	}
}

// The signed-in user keeps LOCALAPPDATA even when it points outside the
// profile, which is exactly what folder redirection and roaming profiles do.
// Deriving from the home there would move an existing user's state root and
// orphan everything already in it.
func TestARedirectedLocalAppDataIsStillHonouredForTheSignedInUser(t *testing.T) {
	home := t.TempDir()
	redirected := filepath.Join(t.TempDir(), "Redirected", "AppData", "Local")
	t.Setenv(userProfileVariable, home)
	t.Setenv("LOCALAPPDATA", redirected)

	if root := UserStateRootFor(home); !strings.HasPrefix(root, filepath.Clean(redirected)) {
		t.Fatalf("state root = %q, want it under the redirected LOCALAPPDATA %q", root, redirected)
	}
}

// A trailing separator is the same directory.
func TestATrailingSeparatorStillMatchesTheSignedInHome(t *testing.T) {
	home := t.TempDir()
	redirected := filepath.Join(t.TempDir(), "Redirected", "AppData", "Local")
	t.Setenv(userProfileVariable, home+string(filepath.Separator))
	t.Setenv("LOCALAPPDATA", redirected)

	if root := UserStateRootFor(home); !strings.HasPrefix(root, filepath.Clean(redirected)) {
		t.Fatalf("state root = %q, want it under %q", root, redirected)
	}
}

// With no LOCALAPPDATA at all, the home is the only source.
func TestAnAbsentLocalAppDataFallsBackToTheHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv(userProfileVariable, home)
	t.Setenv("LOCALAPPDATA", "")

	if root := UserStateRootFor(home); !strings.HasPrefix(root, filepath.Clean(home)) {
		t.Fatalf("state root = %q, want it under %q", root, home)
	}
}
