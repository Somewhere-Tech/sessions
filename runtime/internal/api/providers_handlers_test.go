package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
)

func TestProviderExecutableFindsUserLocalBinOutsideDaemonPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin")
	path := filepath.Join(home, ".local", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 2.1.221\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := providerExecutable("claude")
	if err != nil || got != path {
		t.Fatalf("providerExecutable() = %q, %v; want %q", got, err, path)
	}
	status := localProviderStatus(context.Background(), "claude")
	if !status.Installed || status.Version != "2.1.221" {
		t.Fatalf("provider status = %+v", status)
	}
}

func TestVersionLess(t *testing.T) {
	for _, test := range []struct {
		current string
		latest  string
		want    bool
	}{
		{current: "codex-cli 0.144.5", latest: "0.145.0", want: true},
		{current: "2.1.219 (Claude Code)", latest: "2.1.219", want: false},
		{current: "2.2.0", latest: "2.1.999", want: false},
		{current: "", latest: "2.0.0", want: false},
	} {
		if got := versionLess(test.current, test.latest); got != test.want {
			t.Fatalf("versionLess(%q, %q) = %v, want %v", test.current, test.latest, got, test.want)
		}
	}
}

func TestProviderUpdateRejectsAuthenticatedRemoteClientBeforeMutation(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest(http.MethodPost, "/api/providers/codex/update", strings.NewReader(`{}`))
	request = request.WithContext(context.WithValue(request.Context(), authPrincipalContextKey{}, authPrincipal{
		Kind: ledger.CreatorExternal,
		ID:   "device:paired-client",
		Name: "Paired client",
	}))
	response := httptest.NewRecorder()

	if handled := server.handleProvidersRoute(response, request, ""); !handled {
		t.Fatal("provider update route was not handled")
	}
	if response.Code != http.StatusForbidden ||
		!strings.Contains(response.Body.String(), "require a local Sessions client") {
		t.Fatalf("remote provider update status=%d body=%s", response.Code, response.Body.String())
	}
}
