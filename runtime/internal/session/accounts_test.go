package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// An account is a second subscription with its own provider home. What the
// daemon says about one is the name its owner typed and whether the provider
// has written its login state there — never anything read out of a credential.
func TestAccountCarriesItsLabelAndWhetherTheProviderSignedIn(t *testing.T) {
	root := t.TempDir()
	manager, _, _ := newWorktreeTestManager(t, root)

	created, err := manager.CreateAccount("claude", "work", "Work — team plan")
	if err != nil {
		t.Fatal(err)
	}
	if created.Label != "Work — team plan" || created.SignedIn {
		t.Fatalf("new account = %#v, want the typed label and no login yet", created)
	}
	if _, err := os.Stat(created.Path); err != nil {
		t.Fatalf("account home was not created: %v", err)
	}

	listed := accountNamed(t, manager, "claude", "work")
	if listed.Label != "Work — team plan" || listed.SignedIn {
		t.Fatalf("listed account = %#v", listed)
	}

	// The provider completes its login by writing its own state into that home.
	if err := os.WriteFile(filepath.Join(created.Path, ".credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if listed = accountNamed(t, manager, "claude", "work"); !listed.SignedIn {
		t.Fatalf("account with provider login state = %#v, want signed in", listed)
	}
}

// Codex writes a different file, and a home with neither is simply not signed
// in — including one whose provider keeps its credential somewhere else.
func TestSignedInFollowsEachProvidersOwnLoginState(t *testing.T) {
	root := t.TempDir()
	manager, _, _ := newWorktreeTestManager(t, root)
	codex, err := manager.CreateAccount("codex", "personal", "")
	if err != nil {
		t.Fatal(err)
	}
	if accountNamed(t, manager, "codex", "personal").SignedIn {
		t.Fatal("a fresh Codex home reads as signed in")
	}
	// A Claude marker in a Codex home proves nothing about Codex.
	if err := os.WriteFile(filepath.Join(codex.Path, ".credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if accountNamed(t, manager, "codex", "personal").SignedIn {
		t.Fatal("a Codex account read another provider's file as its login")
	}
	if err := os.WriteFile(filepath.Join(codex.Path, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !accountNamed(t, manager, "codex", "personal").SignedIn {
		t.Fatal("a Codex home with auth.json reads as not signed in")
	}
}

// Forgetting an account takes it off the list and leaves the subscription's
// home alone. Naming it again brings it back with its login intact.
func TestForgettingAnAccountLeavesItsHome(t *testing.T) {
	root := t.TempDir()
	manager, _, _ := newWorktreeTestManager(t, root)
	created, err := manager.CreateAccount("claude", "personal", "Personal")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(created.Path, ".credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.ForgetAccount("claude", "personal"); err != nil {
		t.Fatal(err)
	}
	profiles, err := manager.Profiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range profiles {
		if profile.Tool == "claude" && profile.Name == "personal" {
			t.Fatalf("a forgotten account is still listed: %#v", profile)
		}
	}
	if _, err := os.Stat(filepath.Join(created.Path, ".credentials.json")); err != nil {
		t.Fatalf("forgetting an account touched its login state: %v", err)
	}
	if manager.AccountHomePath("claude", "personal") != created.Path {
		t.Fatalf("the answer does not name the home that was left behind")
	}

	// Adding it again is how it comes back, with what it already had.
	again, err := manager.CreateAccount("claude", "personal", "")
	if err != nil {
		t.Fatal(err)
	}
	if !again.SignedIn || again.Label != "Personal" {
		t.Fatalf("re-registered account = %#v, want its login and label intact", again)
	}
	if !accountNamed(t, manager, "claude", "personal").SignedIn {
		t.Fatal("a re-registered account is missing from the listing")
	}
}

func TestAccountsRefuseNamesAndProvidersThatAreNotOne(t *testing.T) {
	root := t.TempDir()
	manager, _, _ := newWorktreeTestManager(t, root)
	for _, test := range []struct{ tool, name string }{
		{"claude", "Work"}, {"claude", ""}, {"shell", "work"}, {"", "work"},
		{"claude", "../escape"},
	} {
		if _, err := manager.CreateAccount(test.tool, test.name, ""); err == nil {
			t.Errorf("CreateAccount(%q, %q) was accepted", test.tool, test.name)
		}
	}
}

func accountNamed(t *testing.T, manager *Manager, tool, name string) ProfileStatus {
	t.Helper()
	profiles, err := manager.Profiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range profiles {
		if profile.Tool == tool && profile.Name == name {
			return profile
		}
	}
	t.Fatalf("account %s/%s is not listed", tool, name)
	return ProfileStatus{}
}
