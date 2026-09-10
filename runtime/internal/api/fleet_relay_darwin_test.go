//go:build darwin

package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/localnetwork"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

const (
	fleetTestLANEndpoint     = "http://10.129.174.32:8787"
	fleetTestTailnetEndpoint = "http://100.100.20.30:8787"
)

// routedFleetTransport dials named endpoints at a real test server and fails
// the rest with the errno Darwin reports for both a blocked local network and
// a machine that is simply not there.
func routedFleetTransport(t *testing.T, routes map[string]string, refused map[string]bool) func() {
	t.Helper()
	original := fleetRelayTransport
	fleetRelayTransport = &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if target, ok := routes[address]; ok {
				return (&net.Dialer{}).DialContext(ctx, network, target)
			}
			if refused[address] {
				return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
			}
			return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.EHOSTUNREACH}
		},
		ResponseHeaderTimeout: 2 * time.Second,
	}
	return func() { fleetRelayTransport = original }
}

func fleetIdentityServer(t *testing.T, machineID string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/machine" || request.Header.Get("Authorization") != "Bearer "+fleetHostCredential {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(response).Encode(map[string]string{"machine_id": machineID, "name": "B"})
	}))
	t.Cleanup(server.Close)
	return server
}

func fleetMachineRow(t *testing.T, daemon testDaemon) fleetMachineView {
	t.Helper()
	response := serve(t, daemon.handler, http.MethodGet, "/api/fleet/machines", nil, "127.0.0.1:1234", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Machines []fleetMachineView `json:"machines"`
	}
	decodeBody(t, response, &body)
	if len(body.Machines) != 1 {
		t.Fatalf("fleet machines = %+v", body.Machines)
	}
	return body.Machines[0]
}

// EHOSTUNREACH is what macOS returns whether or not the Local Network switch is
// off, so the row keeps the transport failure and the address that produced it
// and offers the permission only as a candidate cause.
func TestFleetLANFailureReportsPossibleCauseWithoutClaimingDenial(t *testing.T) {
	defer routedFleetTransport(t, nil, nil)()
	daemon := newTestDaemon(t)
	saveFleetMachineForTest(t, daemon, fleetSavedMachine{
		MachineID: "machine-b", Name: "B", Endpoint: fleetTestLANEndpoint, Transport: "nearby",
	}, fleetHostCredential)

	row := fleetMachineRow(t, daemon)
	if row.Reachable || row.Reason != localnetwork.Reason {
		t.Fatalf("fleet machine = %+v", row)
	}
	for _, want := range []string{fleetTestLANEndpoint, "no route to host", localnetwork.PossibleCause} {
		if !strings.Contains(row.Message, want) {
			t.Fatalf("message %q does not preserve %q", row.Message, want)
		}
	}
	if got := daemon.handler.lan.state().Permission.Status; got != "not-yet-asked" {
		t.Fatalf("permission = %q, want the unproven state to survive an unreachable dial", got)
	}
	if settings, err := state.LoadSettings(daemon.handler.lan.settingsPath); err == nil && settings.LocalNetworkPermission != "" {
		t.Fatalf("persisted permission = %q, want no recorded verdict", settings.LocalNetworkPermission)
	}
}

// The fallback is the whole point of keeping several endpoints: a working
// tailnet route stays reachable, and it proves nothing about the LAN.
func TestFleetTailnetFallbackSucceedsWithoutProvingLANPermission(t *testing.T) {
	peer := fleetIdentityServer(t, "machine-b")
	defer routedFleetTransport(t, map[string]string{"100.100.20.30:8787": peer.Listener.Addr().String()}, nil)()
	daemon := newTestDaemon(t)
	saveFleetMachineForTest(t, daemon, fleetSavedMachine{
		MachineID: "machine-b", Name: "B", Endpoint: fleetTestLANEndpoint, Transport: "nearby",
		LANEndpoint: fleetTestLANEndpoint, TailnetIPEndpoint: fleetTestTailnetEndpoint,
	}, fleetHostCredential)

	row := fleetMachineRow(t, daemon)
	if !row.Reachable || row.Transport != "tailnet-ip" || row.Endpoint != fleetTestTailnetEndpoint {
		t.Fatalf("fleet machine = %+v", row)
	}
	if row.Reason != "" || row.Message != "" {
		t.Fatalf("reachable machine carried a failure reason: %+v", row)
	}
	if got := daemon.handler.lan.state().Permission.Status; got != "not-yet-asked" {
		t.Fatalf("permission = %q, want a tailnet success to prove nothing about the LAN", got)
	}
}

// The saved primary address is not evidence about the route that actually
// failed: a tailnet failure on a machine whose primary is a LAN address used to
// be reported as a local-network permission problem.
func TestFleetFallbackFailureIsAttributedToTheEndpointItDialled(t *testing.T) {
	defer routedFleetTransport(t, nil, map[string]bool{"10.129.174.32:8787": true})()
	daemon := newTestDaemon(t)
	saveFleetMachineForTest(t, daemon, fleetSavedMachine{
		MachineID: "machine-b", Name: "B", Endpoint: fleetTestLANEndpoint, Transport: "nearby",
		LANEndpoint: fleetTestLANEndpoint, TailnetIPEndpoint: fleetTestTailnetEndpoint,
	}, fleetHostCredential)

	row := fleetMachineRow(t, daemon)
	if row.Reachable || row.Reason != "" || row.Message != "" {
		t.Fatalf("tailnet failure reported as a local-network problem: %+v", row)
	}
	if got := daemon.handler.lan.state().Permission.Status; got != "not-yet-asked" {
		t.Fatalf("permission = %q, want unchanged", got)
	}

	// The row hides the detail, so assert the error itself: it must name the
	// tailnet candidate that actually failed and must not blame the saved LAN
	// primary, which nothing here established anything about.
	ctx, cancel := context.WithTimeout(context.Background(), fleetProbeTimeout)
	defer cancel()
	_, _, err := daemon.handler.selectFleetEndpoint(ctx, fleetSavedMachine{
		MachineID: "machine-b", Name: "B", Endpoint: fleetTestLANEndpoint, Transport: "nearby",
		LANEndpoint: fleetTestLANEndpoint, TailnetIPEndpoint: fleetTestTailnetEndpoint,
	}, fleetHostCredential)
	if err == nil {
		t.Fatal("selectFleetEndpoint succeeded with every candidate failing")
	}
	if !strings.Contains(err.Error(), "reach "+fleetTestTailnetEndpoint+":") {
		t.Fatalf("error %q does not name the endpoint it dialled", err)
	}
	if strings.Contains(err.Error(), fleetTestLANEndpoint) {
		t.Fatalf("error %q blamed the saved LAN primary", err)
	}
}

// A peer that is off answers exactly like a blocked local network, so an
// ordinary refusal must stay an ordinary unreachable row.
func TestFleetUnreachablePeerIsNotAPermissionStory(t *testing.T) {
	defer routedFleetTransport(t, nil, map[string]bool{"10.129.174.32:8787": true})()
	daemon := newTestDaemon(t)
	saveFleetMachineForTest(t, daemon, fleetSavedMachine{
		MachineID: "machine-b", Name: "B", Endpoint: fleetTestLANEndpoint, Transport: "nearby",
	}, fleetHostCredential)

	row := fleetMachineRow(t, daemon)
	if row.Reachable || row.Reason != "" || row.Message != "" {
		t.Fatalf("refused connection reported as a permission problem: %+v", row)
	}
	if got := daemon.handler.lan.state().Permission.Status; got != "not-yet-asked" {
		t.Fatalf("permission = %q, want unchanged", got)
	}
}

// macOS cannot be blocking local access while a LAN peer answers, so a later
// success reconciles the earlier failed observation instead of leaving the user
// chasing a settings pane.
func TestFleetLANSuccessReconcilesEarlierFailedObservation(t *testing.T) {
	restore := routedFleetTransport(t, nil, nil)
	daemon := newTestDaemon(t)
	saveFleetMachineForTest(t, daemon, fleetSavedMachine{
		MachineID: "machine-b", Name: "B", Endpoint: fleetTestLANEndpoint, Transport: "nearby",
	}, fleetHostCredential)
	if row := fleetMachineRow(t, daemon); row.Reachable {
		t.Fatalf("first probe should have failed: %+v", row)
	}
	restore()

	peer := fleetIdentityServer(t, "machine-b")
	defer routedFleetTransport(t, map[string]string{"10.129.174.32:8787": peer.Listener.Addr().String()}, nil)()
	row := fleetMachineRow(t, daemon)
	if !row.Reachable || row.Transport != "lan" {
		t.Fatalf("fleet machine = %+v", row)
	}
	if got := daemon.handler.lan.state().Permission.Status; got != "granted" {
		t.Fatalf("permission = %q, want granted after nearby contact succeeded", got)
	}
	settings, err := state.LoadSettings(daemon.handler.lan.settingsPath)
	if err != nil || settings.LocalNetworkPermission != "granted" {
		t.Fatalf("persisted permission = %q, err=%v", settings.LocalNetworkPermission, err)
	}
}
