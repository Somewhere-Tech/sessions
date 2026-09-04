package fleetaccount

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestTokenStoreIsAtomicPrivateAndRotatesOnlyCompletePairs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet-account.json")
	store := &store{path: path}
	initial := TokenPair{AccessToken: "access-one", RefreshToken: "refresh-one", SessionToken: "session-one"}
	if err := store.update(func(state *persistedState) { state.Tokens = initial }); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("account mode = %o, want 600", info.Mode().Perm())
	}
	if err := store.applyRotation(http.Header{"X-New-Access-Token": {"access-incomplete"}}); err != nil {
		t.Fatal(err)
	}
	state, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Tokens != initial {
		t.Fatalf("incomplete rotation changed tokens: %+v", state.Tokens)
	}
	headers := http.Header{
		"X-New-Access-Token":  {"access-two"},
		"X-New-Refresh-Token": {"refresh-two"},
	}
	if err := store.applyRotation(headers); err != nil {
		t.Fatal(err)
	}
	state, err = store.load()
	if err != nil {
		t.Fatal(err)
	}
	want := TokenPair{AccessToken: "access-two", RefreshToken: "refresh-two", SessionToken: "session-one"}
	if state.Tokens != want {
		t.Fatalf("rotated tokens = %+v, want %+v", state.Tokens, want)
	}
}
