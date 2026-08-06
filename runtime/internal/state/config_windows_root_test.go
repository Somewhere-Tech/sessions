package state

import (
	"path/filepath"
	"strings"
	"testing"
)

// The Windows application root is decided by resolveWindowsAppRoot, which takes
// the environment as arguments so this table runs on every host rather than
// only on Windows. Paths are built with filepath.Join so the separator is
// whatever the host uses.
func TestResolveWindowsAppRoot(t *testing.T) {
	profile := filepath.Join("Users", "real")
	profileLocal := filepath.Join(profile, "AppData", "Local")
	redirected := filepath.Join("Redirected", "real", "AppData", "Local")
	scratch := filepath.Join("scratch", "home")

	for _, testCase := range []struct {
		name         string
		home         string
		signedIn     string
		localAppData string
		want         string
		why          string
	}{
		{
			name:         "the signed-in user gets LOCALAPPDATA",
			home:         profile,
			signedIn:     profile,
			localAppData: profileLocal,
			want:         filepath.Join(profileLocal, "Sessions"),
			why:          "this is where Windows keeps per-user application data",
		},
		{
			name:         "a redirected LOCALAPPDATA is still honoured",
			home:         profile,
			signedIn:     profile,
			localAppData: redirected,
			want:         filepath.Join(redirected, "Sessions"),
			why:          "folder redirection and roaming profiles point it outside the profile; deriving from the home would orphan existing state",
		},
		{
			name:         "another home is not overridden by the environment",
			home:         scratch,
			signedIn:     profile,
			localAppData: profileLocal,
			want:         filepath.Join(scratch, "AppData", "Local", "Sessions"),
			why:          "a test or a scratch daemon passing its own home must not reach the signed-in user's real state",
		},
		{
			name:         "a trailing separator is the same directory",
			home:         profile + string(filepath.Separator),
			signedIn:     profile,
			localAppData: redirected,
			want:         filepath.Join(redirected, "Sessions"),
			why:          "path punctuation must not decide whose state this is",
		},
		{
			name:         "case differences still match",
			home:         strings.ToUpper(profile),
			signedIn:     profile,
			localAppData: redirected,
			want:         filepath.Join(redirected, "Sessions"),
			why:          "Windows paths are case-insensitive",
		},
		{
			name:         "an absent LOCALAPPDATA falls back to the home",
			home:         profile,
			signedIn:     profile,
			localAppData: "",
			want:         filepath.Join(profileLocal, "Sessions"),
			why:          "the home is the only remaining source",
		},
		{
			name:         "an unreadable signed-in home keeps state under the given home",
			home:         scratch,
			signedIn:     "",
			localAppData: profileLocal,
			want:         filepath.Join(scratch, "AppData", "Local", "Sessions"),
			why:          "not knowing who we are is not a reason to write to somebody else's profile",
		},
		{
			name:         "a sibling with a shared prefix is a different home",
			home:         profile + "-other",
			signedIn:     profile,
			localAppData: profileLocal,
			want:         filepath.Join(profile+"-other", "AppData", "Local", "Sessions"),
			why:          "string prefixes are not directory containment",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := resolveWindowsAppRoot(testCase.home, testCase.signedIn, testCase.localAppData)
			if got != testCase.want {
				t.Fatalf("resolveWindowsAppRoot(%q, %q, %q) = %q, want %q — %s",
					testCase.home, testCase.signedIn, testCase.localAppData, got, testCase.want, testCase.why)
			}
		})
	}
}
