package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sessionstate "github.com/somewhere-tech/sessions/runtime/internal/state"
)

// Saved machines are how `sessions --machine NAME` finds another host. Building
// the Unix state layout by hand pointed those files at a
// %USERPROFILE%\.local\state\sessions that nothing on Windows ever writes, so a
// machine registered there could not be reached again from its own CLI.
func TestMachineCredentialPathsFollowThePlatformStateRoot(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	root := sessionstate.UserStateRootFor(home)

	cases := map[string]struct{ got, want string }{
		"registry":  {machineRegistryPath(home), filepath.Join(root, "clients.json")},
		"token":     {savedMachineTokenPath(home, "peer-1"), filepath.Join(root, "clients", "peer-1.token")},
		"client id": {clientIDPath(home), filepath.Join(root, "client-id")},
	}
	for name, test := range cases {
		if test.got != test.want {
			t.Fatalf("%s path = %q, want %q", name, test.got, test.want)
		}
		if !strings.HasPrefix(test.got, root) {
			t.Fatalf("%s path %q escaped the user state root %q", name, test.got, root)
		}
	}

	// machineTokenPathFor stays the only way to build a credential path, so a
	// poisoned registry still cannot steer a write out of the client directory.
	if _, err := machineTokenPathFor(home, "../escape"); err == nil {
		t.Fatal("machineTokenPathFor accepted a traversal id")
	}
}

// The same bug class as the machine registry and the lane ledger: everything
// the CLI writes under the user state root has to derive it, because a literal
// ~/.local/state/sessions is a nonsense location on Windows.
func TestCLIStateFilesFollowThePlatformStateRoot(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	root := sessionstate.UserStateRootFor(home)

	if got, want := fleetPeerHealthPath(home), filepath.Join(root, "fleet-search-health.json"); got != want {
		t.Fatalf("fleet peer health path = %q, want %q", got, want)
	}

	// runnerMetadataTargets falls back to <user state root>/runners exactly as
	// state.stateRootsFromEnv does when SESSIONS_STATE_DIR is unset.
	t.Setenv("SESSIONS_STATE_DIR", "")
	application := &app{home: home}
	if _, err := application.runnerMetadataTargets(); err != nil {
		t.Fatalf("runner metadata scan: %v", err)
	}
	runners := filepath.Join(root, "runners")
	if err := os.MkdirAll(runners, 0o700); err != nil {
		t.Fatal(err)
	}
	const metadata = `{"id":"41000000-0000-4000-8000-000000000001","cwd":"/tmp"}`
	if err := os.WriteFile(filepath.Join(runners, "runner.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	targets, err := application.runnerMetadataTargets()
	if err != nil {
		t.Fatalf("runner metadata scan: %v", err)
	}
	if len(targets) != 1 || targets[0].cwd != "/tmp" {
		t.Fatalf("runner metadata targets = %#v, want the runner under %s", targets, runners)
	}
}
