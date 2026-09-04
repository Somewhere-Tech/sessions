//go:build darwin

package api

import (
	"context"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/localnetwork"
)

func TestFleetMachineReportsLocalNetworkPermissionDenial(t *testing.T) {
	original := fleetRelayTransport
	fleetRelayTransport = &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.EHOSTUNREACH}
		},
		ResponseHeaderTimeout: time.Second,
	}
	defer func() { fleetRelayTransport = original }()

	daemon := newTestDaemon(t)
	saveFleetMachineForTest(t, daemon, fleetSavedMachine{
		MachineID: "machine-b", Name: "B", Endpoint: "http://10.129.174.32:8787", Transport: "nearby",
	}, fleetHostCredential)
	response := serve(t, daemon.handler, http.MethodGet, "/api/fleet/machines", nil, "127.0.0.1:1234", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Machines []fleetMachineView `json:"machines"`
	}
	decodeBody(t, response, &body)
	if len(body.Machines) != 1 || body.Machines[0].Reachable ||
		body.Machines[0].Reason != localnetwork.Reason || body.Machines[0].Message != localnetwork.Message {
		t.Fatalf("fleet machines = %+v", body.Machines)
	}
}
