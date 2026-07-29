//go:build windows

package ledger

import (
	"strings"
	"testing"
)

func TestLocalUserCreatorIDUsesCurrentWindowsSID(t *testing.T) {
	first, err := LocalUserCreatorID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := LocalUserCreatorID()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("local Windows creator identity changed: %q then %q", first, second)
	}
	if !strings.HasPrefix(first, "sid:S-") {
		t.Fatalf("LocalUserCreatorID() = %q, want a Windows SID identity", first)
	}
}
