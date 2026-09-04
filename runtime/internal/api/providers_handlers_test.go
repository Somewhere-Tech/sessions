package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
)

func TestProviderExecutableFindsUserLocalBinOutsideDaemonPath(t *testing.T) {
	previous := providerVersionTimeout
	providerVersionTimeout = 30 * time.Second
	t.Cleanup(func() { providerVersionTimeout = previous })
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

func TestProviderUpdateRejectsOpenAccessClientBeforeMutation(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest(http.MethodPost, "/api/providers/codex/update", strings.NewReader(`{}`))
	request.Host = ""
	request = request.WithContext(context.WithValue(request.Context(), authPrincipalContextKey{}, authPrincipal{
		Kind: ledger.CreatorExternal,
		ID:   "remote:open-access",
		Name: "Remote open access",
	}))
	response := httptest.NewRecorder()

	if handled := server.handleProvidersRoute(response, request, ""); !handled {
		t.Fatal("provider update route was not handled")
	}
	if response.Code != http.StatusForbidden ||
		!strings.Contains(response.Body.String(), "require a local or paired Sessions client") {
		t.Fatalf("remote provider update status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestProviderUpdateAuthority(t *testing.T) {
	for _, test := range []struct {
		name      string
		principal authPrincipal
		want      bool
	}{
		{name: "local client", principal: authPrincipal{Local: true}, want: true},
		{name: "paired device", principal: authPrincipal{HostAdmin: true, ID: "device:paired"}, want: true},
		{name: "master token", principal: authPrincipal{HostAdmin: true, ID: "remote:master-token"}, want: true},
		{name: "open access", principal: authPrincipal{ID: "remote:open-access"}, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := principalMayUpdateProvider(test.principal); got != test.want {
				t.Fatalf("principalMayUpdateProvider(%#v) = %v, want %v", test.principal, got, test.want)
			}
		})
	}
}

func TestProviderStatusIncludesModelChoicesOnlyWhenRequested(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	daemon := newTestDaemon(t)
	daemon.handler.registry = continuationCatalog(daemon.registry)

	plain := serve(t, daemon.handler, http.MethodGet, "/api/providers", nil, "127.0.0.1:1", nil)
	if strings.Contains(plain.Body.String(), `"models"`) {
		t.Fatalf("ordinary provider status unexpectedly loaded model catalogs: %s", plain.Body.String())
	}
	withModels := serve(t, daemon.handler, http.MethodGet, "/api/providers?include_models=1", nil, "127.0.0.1:1", nil)
	if withModels.Code != http.StatusOK ||
		!strings.Contains(withModels.Body.String(), `"displayName":"Fable 5"`) ||
		!strings.Contains(withModels.Body.String(), `"displayName":"GPT Next"`) {
		t.Fatalf("provider models status=%d body=%s", withModels.Code, withModels.Body.String())
	}
}
