package backup

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestBackupConfigAndKeyFollowThePlatformConfigRoot(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	configRoot := state.UserConfigRootFor(home)

	if got, want := ConfigPath(home), filepath.Join(configRoot, "backup.json"); got != want {
		t.Fatalf("ConfigPath() = %q, want %q", got, want)
	}
	if got, want := KeyPath(home), filepath.Join(configRoot, "backup.key"); got != want {
		t.Fatalf("KeyPath() = %q, want %q", got, want)
	}
	// The key must land beside the config on every host: a key resolved from
	// one root and looked up from another reads as ErrWrongKeyOrCorruptedFile
	// for every existing encrypted backup.
	if got, want := keyPathForConfig(ConfigPath(home)), KeyPath(home); got != want {
		t.Fatalf("keyPathForConfig() = %q, want %q", got, want)
	}
	if runtime.GOOS == "windows" && strings.Contains(ConfigPath(home), ".config") {
		t.Fatalf("windows backup config kept the Unix layout: %q", ConfigPath(home))
	}
	if runtime.GOOS != "windows" {
		if got, want := ConfigPath(home), filepath.Join(home, ".config", "sessions", "backup.json"); got != want {
			t.Fatalf("unix backup config moved: got %q, want %q", got, want)
		}
	}
}

// The somewhere CLI owns its own configuration file. Sessions must read it
// where that CLI writes it, so this one is deliberately not platform-adapted.
func TestSomewhereConfigPathStaysTheVendorLocation(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	if got, want := SomewhereConfigPath(home), filepath.Join(home, ".somewhere", "config.json"); got != want {
		t.Fatalf("SomewhereConfigPath() = %q, want %q", got, want)
	}
}
