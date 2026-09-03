package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
	"github.com/somewhere-tech/sessions/runtime/internal/tokenstore"
)

const fleetHostCredential = "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"

func TestFleetRelayListsOnlySavedApprovedMachines(t *testing.T) {
	var remoteRequests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		remoteRequests.Add(1)
		if request.URL.Path != "/api/machine" || request.Header.Get("Authorization") != "Bearer "+fleetHostCredential {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(response).Encode(map[string]string{"machine_id": "machine-b", "name": "B"})
	}))
	defer remote.Close()

	daemon := newTestDaemon(t)
	saveFleetMachineForTest(t, daemon, fleetSavedMachine{
		MachineID: "machine-b", Name: "B", Endpoint: remote.URL, Transport: "nearby",
	}, fleetHostCredential)
	_, phoneToken, err := daemon.handler.pair.devices.create("Phone")
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{"Authorization": []string{"Bearer " + phoneToken}}
	listed := serve(t, daemon.handler, http.MethodGet, "/api/fleet/machines", nil, "192.168.1.50:1234", headers)
	if listed.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listed.Code, listed.Body.String())
	}
	var body struct {
		Machines []fleetMachineView `json:"machines"`
	}
	decodeBody(t, listed, &body)
	if len(body.Machines) != 1 || body.Machines[0].ID != "machine-b" ||
		body.Machines[0].Transport != "lan" || !body.Machines[0].Reachable {
		t.Fatalf("fleet machines = %+v", body.Machines)
	}
	if remoteRequests.Load() != 1 {
		t.Fatalf("remote requests = %d, want one reachability probe", remoteRequests.Load())
	}

	master := http.Header{"Authorization": []string{"Bearer " + testToken}}
	refused := serve(t, daemon.handler, http.MethodGet, "/api/fleet/machines", nil, "192.168.1.50:1234", master)
	if refused.Code != http.StatusForbidden {
		t.Fatalf("master-token list status=%d body=%s", refused.Code, refused.Body.String())
	}
}

func TestFleetRelayStripsCallerCredentialAndPreservesAttribution(t *testing.T) {
	var receivedBody string
	remote := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/sessions/session-b/submit" {
			http.Error(response, "wrong path", http.StatusNotFound)
			return
		}
		if got := request.Header.Get("Authorization"); got != "Bearer "+fleetHostCredential {
			t.Errorf("authorization = %q", got)
		}
		if got := request.Header.Get("Proxy-Authorization"); got != "" {
			t.Errorf("proxy authorization was forwarded: %q", got)
		}
		if got := request.URL.Query().Get("token"); got != "" {
			t.Errorf("query token was forwarded: %q", got)
		}
		if got := request.Header.Get("X-Sessions-Creator-Session"); got != "manager-a" {
			t.Errorf("creator attribution = %q", got)
		}
		if got := request.Header.Get("X-Sessions-Owner-ID"); got != "owner-a" {
			t.Errorf("owner attribution = %q", got)
		}
		encoded, _ := io.ReadAll(request.Body)
		receivedBody = string(encoded)
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusAccepted)
		_, _ = response.Write([]byte(`{"ok":true}`))
	}))
	defer remote.Close()

	daemon := newTestDaemon(t)
	saveFleetMachineForTest(t, daemon, fleetSavedMachine{
		MachineID: "machine-b", Name: "B", Endpoint: remote.URL, Transport: "nearby",
	}, fleetHostCredential)
	_, phoneToken, err := daemon.handler.pair.devices.create("Phone")
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{
		"Authorization":              []string{"Bearer " + phoneToken},
		"Proxy-Authorization":        []string{"Bearer do-not-forward"},
		"X-Sessions-Creator-Session": []string{"manager-a"},
		"X-Sessions-Owner-ID":        []string{"owner-a"},
	}
	response := serve(
		t, daemon.handler, http.MethodPost,
		"/api/fleet/machine-b/api/sessions/session-b/submit?token="+url.QueryEscape(phoneToken)+"&mode=now",
		strings.NewReader(`{"data":"ship it"}`), "192.168.1.50:1234", headers,
	)
	if response.Code != http.StatusAccepted {
		t.Fatalf("relay status=%d body=%s", response.Code, response.Body.String())
	}
	if receivedBody != `{"data":"ship it"}` {
		t.Fatalf("relayed body = %q", receivedBody)
	}
}

func TestFleetRelayStreamsWebSocketMux(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/ws" || request.Header.Get("Authorization") != "Bearer "+fleetHostCredential || request.URL.Query().Get("token") != "" {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		connection, err := websocket.Accept(response, request, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow()
		kind, message, err := connection.Read(request.Context())
		if err != nil {
			return
		}
		_ = connection.Write(request.Context(), kind, message)
	}))
	defer remote.Close()

	daemon := newTestDaemon(t)
	saveFleetMachineForTest(t, daemon, fleetSavedMachine{
		MachineID: "machine-b", Name: "B", Endpoint: remote.URL, Transport: "nearby",
	}, fleetHostCredential)
	relay := httptest.NewServer(daemon.handler)
	defer relay.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	target := "ws" + strings.TrimPrefix(relay.URL, "http") + "/api/fleet/machine-b/ws?mux=1&token=phone-credential"
	connection, response, err := websocket.Dial(ctx, target, nil)
	if err != nil {
		if response != nil {
			t.Fatalf("dial relay websocket: %v (HTTP %d)", err, response.StatusCode)
		}
		t.Fatalf("dial relay websocket: %v", err)
	}
	defer connection.CloseNow()
	if err := connection.Write(ctx, websocket.MessageText, []byte(`{"type":"ping"}`)); err != nil {
		t.Fatal(err)
	}
	_, echoed, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(echoed) != `{"type":"ping"}` {
		t.Fatalf("echoed websocket frame = %q", echoed)
	}
}

func TestFleetRelayRefusesUnknownMachineBeforeOutboundRequest(t *testing.T) {
	var remoteRequests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		remoteRequests.Add(1)
	}))
	defer remote.Close()

	daemon := newTestDaemon(t)
	saveFleetMachineForTest(t, daemon, fleetSavedMachine{
		MachineID: "machine-b", Name: "B", Endpoint: remote.URL, Transport: "nearby",
	}, fleetHostCredential)
	response := serve(
		t, daemon.handler, http.MethodGet, "/api/fleet/not-approved/api/health", nil,
		"127.0.0.1:1234", nil,
	)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown-machine status=%d body=%s", response.Code, response.Body.String())
	}
	if remoteRequests.Load() != 0 {
		t.Fatalf("unknown machine caused %d outbound requests", remoteRequests.Load())
	}
}

func saveFleetMachineForTest(t *testing.T, daemon testDaemon, machine fleetSavedMachine, credential string) {
	t.Helper()
	registry := fleetMachineRegistry{Version: fleetRegistryVersion, Machines: []fleetSavedMachine{machine}}
	encoded, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	root := daemon.handler.fleetStateRoot()
	if err := os.WriteFile(filepath.Join(root, "clients.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tokenstore.WriteSecret(filepath.Join(root, "clients", machine.MachineID+".token"), credential); err != nil {
		t.Fatal(err)
	}
}
