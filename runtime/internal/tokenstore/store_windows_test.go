//go:build windows

package tokenstore

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

const fixtureToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestWindowsTokenStoreCreatesProtectedTokenWithOwnerACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "token")
	value, err := ReadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if !Valid(value) {
		t.Fatalf("generated token is invalid: %q", value)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(value)) {
		t.Fatal("protected token file contains the plaintext daemon token")
	}
	if reread, err := Read(path); err != nil || reread != value {
		t.Fatalf("protected token reread = %q, %v", reread, err)
	}
	assertOwnerAndSystemDACL(t, path)
	assertOwnerAndSystemDACL(t, filepath.Dir(path))
}

func TestWindowsTokenStoreMigratesLegacyPlaintextAfterVerification(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "token")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fixtureToken), 0o600); err != nil {
		t.Fatal(err)
	}
	value, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if value != fixtureToken {
		t.Fatalf("migrated token = %q", value)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(fixtureToken)) {
		t.Fatal("legacy plaintext remained after verified DPAPI migration")
	}
	assertOwnerAndSystemDACL(t, path)
	assertOwnerAndSystemDACL(t, filepath.Dir(path))
}

func TestWindowsTokenStoreRejectsCorruptionWithoutRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "token")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	const damaged = `{"version":1,"protected":"not-ciphertext"}`
	if err := os.WriteFile(path, []byte(damaged), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadOrCreate(path); err == nil ||
		!strings.Contains(err.Error(), "remove it explicitly only if you intend to rotate") {
		t.Fatalf("corrupt store error = %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != damaged {
		t.Fatalf("corrupt store was rotated or rewritten: %q", raw)
	}
}

func TestWindowsTokenStoreBindsCiphertextToItsPath(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "one", "token")
	second := filepath.Join(root, "two", "token")
	if err := os.MkdirAll(filepath.Dir(first), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte(fixtureToken), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(first); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(second), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(second); err == nil ||
		!strings.Contains(err.Error(), "signed-in Windows user") {
		t.Fatalf("copied ciphertext error = %v", err)
	}
}

func assertOwnerAndSystemDACL(t *testing.T, path string) {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("daemon-token DACL still inherits access")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if dacl == nil || dacl.AceCount != 2 {
		t.Fatalf("daemon-token DACL entries = %v; want current user and LocalSystem", dacl)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	foundUser := false
	foundSystem := false
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Fatalf("daemon-token ACE %d is not an allow entry", index)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		foundUser = foundUser || sid.Equals(user.User.Sid)
		foundSystem = foundSystem || sid.Equals(system)
	}
	if !foundUser || !foundSystem {
		t.Fatalf("daemon-token DACL user=%t system=%t", foundUser, foundSystem)
	}
}
