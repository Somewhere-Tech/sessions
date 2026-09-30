package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestRelaySettingsStayLocalAndAdvertiseMachineEndpoint(t *testing.T) {
	daemon := newTestDaemon(t)
	daemon.config.SettingsPath = filepath.Join(daemon.root, "settings.json")
	daemon.handler = New(daemon.config, daemon.registry)
	body, _ := json.Marshal(map[string]string{"url": "https://relay.example"})
	saved := serve(t, daemon.handler, http.MethodPut, "/api/relay", bytes.NewReader(body), "127.0.0.1:4567",
		http.Header{"Content-Type": {"application/json"}})
	if saved.Code != http.StatusOK || !strings.Contains(saved.Body.String(), `"source":"settings"`) {
		t.Fatalf("save relay = %d %s", saved.Code, saved.Body.String())
	}
	endpoint := daemon.handler.fleetAccountEndpoints().Relay
	if endpoint != "https://relay.example/m/"+daemon.handler.identity.ID {
		t.Fatalf("advertised relay endpoint = %q", endpoint)
	}
	remote := serve(t, daemon.handler, http.MethodGet, "/api/relay", nil, "198.51.100.4:4567",
		http.Header{"Authorization": {"Bearer " + testToken}})
	if remote.Code != http.StatusForbidden {
		t.Fatalf("remote relay settings = %d, want 403", remote.Code)
	}
}

func TestRelaySettingsRejectPublicPlainHTTP(t *testing.T) {
	daemon := newTestDaemon(t)
	daemon.config.SettingsPath = filepath.Join(daemon.root, "settings.json")
	daemon.handler = New(daemon.config, daemon.registry)
	body, _ := json.Marshal(map[string]string{"url": "http://relay.example:8899"})
	response := serve(t, daemon.handler, http.MethodPut, "/api/relay", bytes.NewReader(body), "127.0.0.1:4567",
		http.Header{"Content-Type": {"application/json"}})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("plain HTTP relay = %d %s", response.Code, response.Body.String())
	}
}

func TestRelayEnvironmentRejectsPublicPlainHTTP(t *testing.T) {
	daemon := newTestDaemon(t)
	daemon.config.SettingsPath = filepath.Join(daemon.root, "settings.json")
	daemon.config.FleetRelayEndpoint = "http://relay.example:8899"
	daemon.handler = New(daemon.config, daemon.registry)
	if _, _, err := daemon.handler.configuredRelayBase(); err == nil {
		t.Fatal("public plain-HTTP environment relay was accepted")
	}
}

func TestRelayClientEndpointValidation(t *testing.T) {
	if got, err := validateRelayClientEndpoint("http://localhost:8899/m/machine-a"); err != nil || got == "" {
		t.Fatalf("loopback relay endpoint = %q, %v", got, err)
	}
	if _, err := validateRelayClientEndpoint("https://relay.example/%zz"); err == nil {
		t.Fatal("malformed relay endpoint was accepted")
	}
}
