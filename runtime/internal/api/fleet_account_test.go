package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFleetAccountStatusAndKeyStayLocal(t *testing.T) {
	daemon := newTestDaemon(t)
	status := serve(t, daemon.handler, http.MethodGet, "/api/account", nil, "127.0.0.1:4567", nil)
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"signed_in":false`) {
		t.Fatalf("local account status = %d %s", status.Code, status.Body.String())
	}
	remote := serve(t, daemon.handler, http.MethodGet, "/api/account", nil, "198.51.100.4:4567",
		http.Header{"Authorization": {"Bearer " + testToken}})
	if remote.Code != http.StatusForbidden {
		t.Fatalf("remote account status = %d, want 403", remote.Code)
	}
	key := serve(t, daemon.handler, http.MethodGet, "/api/account/key", nil, "127.0.0.1:4567", nil)
	if key.Code != http.StatusOK || strings.Contains(key.Body.String(), "private") {
		t.Fatalf("account key response = %d %s", key.Code, key.Body.String())
	}
	info, err := os.Stat(filepath.Join(daemon.config.StateRoot, "fleet-machine-key.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("machine key mode = %o, want 600", info.Mode().Perm())
	}
}

func TestHealthReportsOnlyAccountPresence(t *testing.T) {
	daemon := newTestDaemon(t)
	health := serve(t, daemon.handler, http.MethodGet, "/api/health", nil, "198.51.100.4:4567", nil)
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d", health.Code)
	}
	body := health.Body.String()
	if !strings.Contains(body, `"account":{"signedIn":false}`) {
		t.Fatalf("health account projection missing: %s", body)
	}
	for _, forbidden := range []string{"access_token", "refresh_token", "machine_public_key", "email"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("health exposed %q: %s", forbidden, body)
		}
	}
}
