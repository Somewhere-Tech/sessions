package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
		{"claude", "Work"}, {"shell", "work"}, {"", "work"},
		{"claude", "../escape"},
	} {
		if _, err := manager.CreateAccount(test.tool, test.name, ""); err == nil {
			t.Errorf("CreateAccount(%q, %q) was accepted", test.tool, test.name)
		}
	}
}

// Renaming changes the nickname and nothing else: the account keeps its ID,
// its provider home, its login state, its checked identity and its history.
func TestRenamingAnAccountKeepsEverythingButItsNickname(t *testing.T) {
	root := t.TempDir()
	manager, _, _ := newWorktreeTestManager(t, root)
	created, err := manager.CreateAccount("claude", "work", "Work")
	if err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(created.Path, "projects", "chat.jsonl")
	if err := os.MkdirAll(filepath.Dir(history), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{history: "{}\n", filepath.Join(created.Path, ".credentials.json"): "{}"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	identity := &AccountIdentity{Email: "me@example.com", Plan: "max", CheckedAt: 1}
	if err := writeAccountSidecar(manager.config.UserStateRoot, "claude", "work", accountSidecar{Identity: identity, Label: "Work"}); err != nil {
		t.Fatal(err)
	}

	renamed, err := manager.RenameAccount("claude", "work", "Team plan ✦")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Name != "work" || renamed.Path != created.Path || renamed.Label != "Team plan ✦" || !renamed.SignedIn ||
		renamed.Identity == nil || renamed.Identity.Email != "me@example.com" {
		t.Fatalf("renamed account = %#v", renamed)
	}
	listed := accountNamed(t, manager, "claude", "work")
	if listed.Label != "Team plan ✦" || listed.Identity == nil || listed.Identity.Email != "me@example.com" {
		t.Fatalf("listed after rename = %#v, want the new nickname persisted with the identity", listed)
	}
	if raw, err := os.ReadFile(history); err != nil || string(raw) != "{}\n" {
		t.Fatalf("renaming touched the account history: %q %v", raw, err)
	}

	cleared, err := manager.RenameAccount("claude", "work", "")
	if err != nil || cleared.Label != "" || accountNamed(t, manager, "claude", "work").Label != "" {
		t.Fatalf("clearing the nickname = %#v %v", cleared, err)
	}
}

func TestRenamingRefusesUnknownAccountsAndUnreadableNicknames(t *testing.T) {
	root := t.TempDir()
	manager, _, _ := newWorktreeTestManager(t, root)
	if _, err := manager.CreateAccount("codex", "personal", ""); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{strings.Repeat("a", MaxAccountLabelLength+1), "two\nlines", "tab\there", "\xff"} {
		if _, err := manager.RenameAccount("codex", "personal", label); err == nil {
			t.Errorf("RenameAccount accepted %q", label)
		}
		if _, err := manager.CreateAccount("codex", "other", label); err == nil {
			t.Errorf("CreateAccount accepted label %q", label)
		}
	}
	if _, err := manager.RenameAccount("codex", "personal", strings.Repeat("é", MaxAccountLabelLength)); err != nil {
		t.Errorf("a %d-character nickname was refused: %v", MaxAccountLabelLength, err)
	}
	if _, err := manager.RenameAccount("claude", "personal", "x"); err == nil {
		t.Error("renaming an account that does not exist was accepted")
	}
	if err := manager.ForgetAccount("codex", "personal"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RenameAccount("codex", "personal", "x"); err == nil {
		t.Error("renaming a forgotten account put it back on the list")
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
