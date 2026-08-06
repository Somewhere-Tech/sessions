package main

import (
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
