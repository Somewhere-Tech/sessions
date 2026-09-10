package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/tokenstore"
)

// saveFleetMachinesForTest writes a whole saved-machine registry, which is what
// a host actually has: several peers, not one.
func saveFleetMachinesForTest(t *testing.T, daemon testDaemon, machines []fleetSavedMachine, credential string) {
	t.Helper()
	encoded, err := json.Marshal(fleetMachineRegistry{Version: fleetRegistryVersion, Machines: machines})
	if err != nil {
		t.Fatal(err)
	}
	root := daemon.handler.fleetStateRoot()
	if err := os.WriteFile(filepath.Join(root, "clients.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, machine := range machines {
		if err := tokenstore.WriteSecret(filepath.Join(root, "clients", machine.MachineID+".token"), credential); err != nil {
			t.Fatal(err)
		}
	}
}

// fleetPeerServer answers the identity and health probes a reachable peer owes.
func fleetPeerServer(t *testing.T, machineID string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+fleetHostCredential {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/api/machine":
			_ = json.NewEncoder(response).Encode(map[string]string{"machine_id": machineID, "name": machineID})
		case "/api/health":
			_ = json.NewEncoder(response).Encode(map[string]any{"ok": true, "name": "sessionsd"})
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// onlyReachable routes the named addresses at real listeners and refuses every
// other dial, so an offline peer is deterministic and instant.
func onlyReachable(t *testing.T, routes map[string]bool) func() {
	t.Helper()
	original := fleetRelayTransport
	fleetRelayTransport = &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if routes[address] {
				return (&net.Dialer{}).DialContext(ctx, network, address)
			}
			return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
		},
		ResponseHeaderTimeout: 2 * time.Second,
	}
	return func() { fleetRelayTransport = original }
}

func fleetMachineRows(t *testing.T, daemon testDaemon) []fleetMachineView {
	t.Helper()
	response := serve(t, daemon.handler, http.MethodGet, "/api/fleet/machines", nil, "127.0.0.1:1234", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("fleet listing = %d %s", response.Code, response.Body.String())
	}
	var body struct {
		Machines []fleetMachineView `json:"machines"`
	}
	decodeBody(t, response, &body)
	return body.Machines
}

// A machine claimed from the account directory is saved with the addresses that
// directory published, and nothing checks them against this host's transport
// rules first (cmd/sessions/machines.go claimAccountMachines). One address this
// host cannot use must therefore cost that one machine — not the whole family.
func TestOneUnusableSavedAddressDoesNotHideTheRestOfTheFleet(t *testing.T) {
	peer := fleetPeerServer(t, "machine-b")
	defer onlyReachable(t, map[string]bool{peer.Listener.Addr().String(): true})()

	daemon := newTestDaemon(t)
	saveFleetMachinesForTest(t, daemon, []fleetSavedMachine{
		{MachineID: "machine-b", Name: "Mac B", Endpoint: peer.URL, LANEndpoint: peer.URL, Transport: "nearby"},
		{
			MachineID: "machine-d", Name: "Mac D", Transport: "tailnet",
			// A directory-published HTTPS name that is not a tailnet name.
			Endpoint: "https://mac-d.example.com", TailnetEndpoint: "https://mac-d.example.com",
		},
	}, fleetHostCredential)

	rows := fleetMachineRows(t, daemon)
	if len(rows) != 2 {
		t.Fatalf("fleet rows = %+v, want both machines listed", rows)
	}
	usable, unusable := rows[0], rows[1]
	if usable.ID != "machine-b" || !usable.Reachable || usable.Transport != "lan" {
		t.Fatalf("healthy peer = %+v", usable)
	}
	if unusable.ID != "machine-d" || unusable.Reachable {
		t.Fatalf("unusable peer = %+v", unusable)
	}
	if unusable.Reason != fleetEndpointUnusableReason || !strings.Contains(unusable.Message, "machine-d") {
		t.Fatalf("unusable peer says %q / %q", unusable.Reason, unusable.Message)
	}

	// Routing to the healthy peer keeps working while that row exists.
	viaB := serve(t, daemon.handler, http.MethodGet, "/api/fleet/machine-b/api/health", nil, "127.0.0.1:1234", nil)
	if viaB.Code != http.StatusOK || !strings.Contains(viaB.Body.String(), `"ok":true`) {
		t.Fatalf("relay to the healthy peer = %d %s", viaB.Code, viaB.Body.String())
	}
}

// The unusable machine itself is a bad gateway, not a broken host: this daemon
// is fine, it simply has no address for that peer that it can dial.
func TestRelayToAnUnusableOrOfflinePeerIsABadGatewayNotAHostFailure(t *testing.T) {
	peer := fleetPeerServer(t, "machine-b")
	defer onlyReachable(t, map[string]bool{peer.Listener.Addr().String(): true})()

	daemon := newTestDaemon(t)
	saveFleetMachinesForTest(t, daemon, []fleetSavedMachine{
		{MachineID: "machine-b", Name: "Mac B", Endpoint: peer.URL, LANEndpoint: peer.URL, Transport: "nearby"},
		{
			MachineID: "machine-d", Name: "Mac D", Transport: "tailnet",
			Endpoint: "https://mac-d.example.com", TailnetEndpoint: "https://mac-d.example.com",
		},
		{
			// Offline, and saved with more than one route so selection has to
			// try them all before giving up.
			MachineID: "machine-e", Name: "Mac E", Transport: "nearby",
			Endpoint: "http://10.9.9.9:8787", LANEndpoint: "http://10.9.9.9:8787",
			TailnetIPEndpoint: "http://100.100.9.9:8787",
		},
	}, fleetHostCredential)

	unusable := serve(t, daemon.handler, http.MethodGet, "/api/fleet/machine-d/api/health", nil, "127.0.0.1:1234", nil)
	if unusable.Code != http.StatusBadGateway {
		t.Fatalf("relay to an unusable peer = %d %s", unusable.Code, unusable.Body.String())
	}
	var body map[string]any
	decodeBody(t, unusable, &body)
	if body["reason"] != fleetEndpointUnusableReason {
		t.Fatalf("relay to an unusable peer = %#v", body)
	}

	offline := serve(t, daemon.handler, http.MethodGet, "/api/fleet/machine-e/api/health", nil, "127.0.0.1:1234", nil)
	if offline.Code != http.StatusBadGateway {
		t.Fatalf("relay to an offline peer = %d %s", offline.Code, offline.Body.String())
	}
	if !strings.Contains(offline.Body.String(), "10.9.9.9:8787") && !strings.Contains(offline.Body.String(), "100.100.9.9:8787") {
		t.Fatalf("relay to an offline peer did not name a route it tried: %s", offline.Body.String())
	}
}

// A registry this host cannot parse at all is still the host's own problem.
func TestUnreadableRegistryStaysAHostFailure(t *testing.T) {
	daemon := newTestDaemon(t)
	root := daemon.handler.fleetStateRoot()
	if err := os.WriteFile(filepath.Join(root, "clients.json"), []byte(`{"version":99,"machines":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	response := serve(t, daemon.handler, http.MethodGet, "/api/fleet/machines", nil, "127.0.0.1:1234", nil)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("unsupported registry version = %d %s", response.Code, response.Body.String())
	}
}
