package migrate

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestValidateRemoteURLAcceptsTheFormsGitRemoteGetURLReturns(t *testing.T) {
	for _, remote := range []string{
		"https://github.com/org/repo.git",
		"http://git.internal:3000/org/repo.git",
		"https://user:token@github.com/org/repo.git",
		"ssh://git@github.com:22/org/repo.git",
		"git@github.com:org/repo.git",
		"git://git.kernel.org/pub/scm/git/git.git",
		"file:///srv/git/repo.git",
		"/Volumes/shared/repo.git",
	} {
		if err := validateRemoteURL(remote); err != nil {
			t.Errorf("validateRemoteURL(%q) = %v, want a legitimate remote to be accepted", remote, err)
		}
	}
}

func TestValidateRemoteURLRejectsCommandBearingAndUnknownTransports(t *testing.T) {
	for _, test := range []struct{ remote, why string }{
		{"ext::sh -c 'touch /tmp/pwned'", "the reported ext:: remote-helper payload"},
		{"EXT::sh -c id", "an uppercase ext:: helper"},
		{"fd::7/repo", "an fd:: helper"},
		{"transport::whatever", "any other remote helper"},
		{"::/repo", "an empty helper name"},
		{"ftp://example.com/repo.git", "an unlisted transport"},
		{"ftps://example.com/repo.git", "an unlisted transport"},
		{"--upload-pack=touch /tmp/pwned", "a URL git would read as an option"},
		{"-o ProxyCommand=id", "a URL git would read as an option"},
		{"https://example.com/repo.git\nfetch = x", "an embedded newline"},
		{"repo.git", "a bare relative path"},
		{"../../etc/passwd", "a relative traversal"},
		{"", "an empty remote"},
	} {
		err := validateRemoteURL(test.remote)
		if err == nil {
			t.Errorf("validateRemoteURL(%q) accepted %s", test.remote, test.why)
			continue
		}
		if !strings.Contains(err.Error(), "retry the move") && test.remote != "" {
			t.Errorf("validateRemoteURL(%q) = %v, want an instructional next action", test.remote, err)
		}
	}
}

// An error that echoes the remote verbatim would print an embedded token into
// the operator's terminal and the daemon log.
func TestRemoteURLErrorRedactsCredentials(t *testing.T) {
	err := validateRemoteURL("ftp://user:sup3rsecret@example.com/repo.git")
	if err == nil {
		t.Fatal("validateRemoteURL accepted an ftp remote")
	}
	if strings.Contains(err.Error(), "sup3rsecret") {
		t.Fatalf("err = %v, want the password stripped", err)
	}
	if !strings.Contains(err.Error(), "user@example.com") {
		t.Fatalf("err = %v, want the remote still identifiable", err)
	}
}

func TestGitTransportOptionsPinRemoteHelpersOff(t *testing.T) {
	options := gitTransportOptions()
	if !slices.Contains(options, "protocol.ext.allow=never") {
		t.Fatalf("gitTransportOptions() = %#v, want protocol.ext.allow pinned off", options)
	}
	if index := slices.Index(options, "protocol.ext.allow=never"); index == 0 || options[index-1] != "-c" {
		t.Fatalf("gitTransportOptions() = %#v, want the setting introduced by -c", options)
	}
}

// End to end through EnsureWorkspace: a hostile remote must be refused before
// git is invoked at all, and must leave nothing behind on disk.
func TestEnsureWorkspaceRefusesAHostileRemoteBeforeCloning(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "checkout")
	marker := filepath.Join(root, "pwned")
	workspace := Workspace{
		Git:       true,
		Root:      target,
		RemoteURL: "ext::sh -c 'touch " + marker + "'",
		Branch:    "main",
		Revision:  "0123456789abcdef0123456789abcdef01234567",
	}
	err := EnsureWorkspace(context.Background(), filepath.Join(target, "sub"), workspace)
	if err == nil {
		t.Fatal("EnsureWorkspace accepted an ext:: remote")
	}
	if !strings.Contains(err.Error(), "remote helper") {
		t.Fatalf("err = %v, want the remote-helper rejection", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the ext:: payload executed")
	}
	if _, statErr := os.Stat(target); statErr == nil {
		t.Fatal("EnsureWorkspace created the target root for a rejected remote")
	}
}
