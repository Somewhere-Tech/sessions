package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/fleetaccount"
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

func TestAccountClaimIssuesNormalAcknowledgedDeviceCredential(t *testing.T) {
	directory := map[string]fleetaccount.Machine{}
	cloud := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/auth-token/magic-link/verify":
			_ = json.NewEncoder(response).Encode(map[string]any{
				"user":  map[string]string{"id": "owner", "email": "owner@example.com"},
				"token": "access", "refresh_token": "refresh", "session_token": "session",
			})
		case "/api/machines/register":
			var registration fleetaccount.Registration
			_ = json.NewDecoder(request.Body).Decode(&registration)
			directory[registration.MachineID] = fleetaccount.Machine{
				ID: registration.MachineID, Name: registration.Name,
				MachinePublicKey: registration.MachinePublicKey, EndpointsJSON: registration.EndpointsJSON,
			}
			_ = json.NewEncoder(response).Encode(map[string]bool{"ok": true})
		case "/api/machines/index":
			machines := make([]fleetaccount.Machine, 0, len(directory))
			for _, machine := range directory {
				machines = append(machines, machine)
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"machines": machines})
		default:
			http.NotFound(response, request)
		}
	}))
	defer cloud.Close()

	daemon := newTestDaemon(t)
	daemon.config.FleetURL = cloud.URL
	daemon.handler = New(daemon.config, daemon.registry)
	login := serve(t, daemon.handler, http.MethodPost, "/api/account/verify",
		strings.NewReader(`{"token":"host-code"}`), "127.0.0.1:4567", nil)
	if login.Code != http.StatusOK {
		t.Fatalf("host login = %d %s", login.Code, login.Body.String())
	}

	root := t.TempDir()
	requester, err := fleetaccount.New(fleetaccount.Options{
		BaseURL: cloud.URL, AccountPath: filepath.Join(root, "account.json"),
		KeyPath: filepath.Join(root, "key.json"), MachineID: "phone-device", MachineName: "Uzair's phone",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := requester.VerifyMagicLink(context.Background(), "phone-code"); err != nil {
		t.Fatal(err)
	}
	claim, err := requester.CreateAccountClaim(daemon.handler.identity.ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(claim)
	issued := serve(t, daemon.handler, http.MethodPost, fleetaccount.AccountClaimPath,
		bytes.NewReader(encoded), "198.51.100.20:4567", http.Header{"Content-Type": {"application/json"}})
	if issued.Code != http.StatusCreated {
		t.Fatalf("account claim = %d %s", issued.Code, issued.Body.String())
	}
	var credential pairingClaimResponse
	decodeBody(t, issued, &credential)
	if credential.Token == "" || credential.Name != "Uzair's phone" || credential.MachineID != daemon.handler.identity.ID {
		t.Fatalf("issued credential = %+v", credential)
	}
	authorized := serve(t, daemon.handler, http.MethodGet, "/api/sessions", nil, "198.51.100.20:4567",
		http.Header{"Authorization": {"Bearer " + credential.Token}})
	if authorized.Code != http.StatusOK {
		t.Fatalf("issued credential did not acknowledge: %d %s", authorized.Code, authorized.Body.String())
	}
	devices := serve(t, daemon.handler, http.MethodGet, "/api/devices", nil, "127.0.0.1:4567", nil)
	if devices.Code != http.StatusOK || !strings.Contains(devices.Body.String(), "Uzair's phone") {
		t.Fatalf("account device was not recorded: %d %s", devices.Code, devices.Body.String())
	}
}
