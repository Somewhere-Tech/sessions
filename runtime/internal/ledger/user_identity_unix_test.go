//go:build !windows

package ledger

import (
	"os"
	"strconv"
	"testing"
)

func TestLocalUserCreatorIDUsesUnixUID(t *testing.T) {
	got, err := LocalUserCreatorID()
	if err != nil {
		t.Fatal(err)
	}
	want := "uid:" + strconv.Itoa(os.Getuid())
	if got != want {
		t.Fatalf("LocalUserCreatorID() = %q, want %q", got, want)
	}
}
