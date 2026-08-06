package state

import (
	"path/filepath"
	"strings"
)

// resolveWindowsAppRoot decides the per-user application root on Windows.
//
// It lives in a file with no build tag, and takes the environment as arguments
// rather than reading it, so the decision can be exercised on any host. Only
// Windows calls it; the wiring that supplies the arguments is in
// config_windows.go.
//
// LOCALAPPDATA is authoritative for the signed-in user and must stay so: it is
// where Windows actually keeps that data, and under folder redirection or a
// roaming profile it legitimately points outside the profile directory.
// Deriving from the home instead would move an existing user's state root and
// orphan everything already in it.
//
// But it names one specific profile, so it is only right for that profile's
// home. A caller passing some other home means it -- a test with a temp
// directory, or a scratch daemon started per the isolation recipe in AGENTS.md
// -- and following LOCALAPPDATA there aims both at the signed-in user's real
// state. That is the same hole SESSIONS_STATE_DIR had on Unix, and it fails
// silently, because the wrong paths still resolve and still work.
func resolveWindowsAppRoot(home, signedInHome, localAppData string) string {
	if strings.TrimSpace(localAppData) == "" || !sameDirectory(home, signedInHome) {
		localAppData = filepath.Join(home, "AppData", "Local")
	}
	return filepath.Join(localAppData, "Sessions")
}

// sameDirectory compares two paths after cleaning, so a trailing separator or a
// "." segment cannot decide it. Windows paths are case-insensitive. An empty
// side is never a match: an unreadable home keeps state inside the caller's own
// directory rather than reaching for somebody else's.
func sameDirectory(left, right string) bool {
	if strings.TrimSpace(left) == "" || strings.TrimSpace(right) == "" {
		return false
	}
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}
