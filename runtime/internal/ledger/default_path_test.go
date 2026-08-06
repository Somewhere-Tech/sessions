package ledger

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// The ledger is where every destructive intent is recorded before it happens.
// Deriving it from a hardcoded ~/Library path put it in a literal
// C:\Users\<user>\Library\Application Support tree on Windows.
func TestDefaultPathUsesThePlatformStateRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	got, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(state.UserStateRootFor(home), "ledger", "lanes.sqlite3")
	if got != want {
		t.Fatalf("DefaultPath() = %q, want %q", got, want)
	}
	if goruntime.GOOS != "darwin" && strings.Contains(got, "Application Support") {
		t.Fatalf("DefaultPath() kept the macOS layout off macOS: %q", got)
	}
}

// An installation that already has the pre-parity macOS ledger keeps using it.
// Starting an empty ledger beside it would discard the durable record of every
// lane that machine has run.
func TestDefaultPathAdoptsAnExistingLegacyDarwinLedger(t *testing.T) {
	if goruntime.GOOS != "darwin" {
		t.Skip("the legacy ledger location only ever existed on macOS")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacy := filepath.Join(home, "Library", "Application Support", "sessions", "ledger", "lanes.sqlite3")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("existing ledger"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != legacy {
		t.Fatalf("DefaultPath() = %q, want the existing ledger %q", got, legacy)
	}
}

// Once the ledger exists at the platform location it wins, so a leftover
// legacy file cannot pull an already-migrated install backwards.
func TestDefaultPathPrefersThePlatformLedgerWhenBothExist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	current := filepath.Join(state.UserStateRootFor(home), "ledger", "lanes.sqlite3")
	legacy := filepath.Join(home, "Library", "Application Support", "sessions", "ledger", "lanes.sqlite3")
	for _, path := range []string{current, legacy} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("ledger"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != current {
		t.Fatalf("DefaultPath() = %q, want %q", got, current)
	}
}
