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
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/tokenstore"
)

// peerDialer answers each saved address in one of three ways, and records the
// order and timing of every dial the relay attempts.
type peerDialer struct {
	mu       sync.Mutex
	attempts []peerAttempt
	reachAt  map[string]string // address -> real listener to connect to
	stall    map[string]time.Duration
	start    time.Time
}

type peerAttempt struct {
	address string
	at      time.Duration
	outcome string
}

func (d *peerDialer) record(address, outcome string) {
	d.mu.Lock()
	d.attempts = append(d.attempts, peerAttempt{address: address, at: time.Since(d.start), outcome: outcome})
	d.mu.Unlock()
}

func (d *peerDialer) install(t *testing.T) func() {
	t.Helper()
	original := fleetRelayTransport
	d.start = time.Now()
	fleetRelayTransport = &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if target, ok := d.reachAt[address]; ok {
				d.record(address, "connected")
				return (&net.Dialer{}).DialContext(ctx, network, target)
			}
			if wait, ok := d.stall[address]; ok {
				// A machine that is powering down or mid-install: the address is
				// routed and nothing answers. The production transport dials
				// through net.Dialer{Timeout: fleetDialTimeout}, so a hung dial
				// ends there; this fixture applies the same cap rather than
				// hanging for the whole test.
				if wait > fleetDialTimeout {
					wait = fleetDialTimeout
				}
				select {
				case <-time.After(wait):
					d.record(address, "dial timeout")
					return nil, &net.OpError{Op: "dial", Net: network, Err: os.ErrDeadlineExceeded}
				case <-ctx.Done():
					d.record(address, "cancelled")
					return nil, ctx.Err()
				}
			}
			d.record(address, "refused")
			return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
		},
		ResponseHeaderTimeout: 30 * time.Second,
	}
	return func() { fleetRelayTransport = original }
}

func savePeers(t *testing.T, daemon testDaemon, machines []fleetSavedMachine) {
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
		if err := tokenstore.WriteSecret(filepath.Join(root, "clients", machine.MachineID+".token"), fleetHostCredential); err != nil {
			t.Fatal(err)
		}
	}
}

func healthyPeer(t *testing.T, machineID string, requests *atomic.Int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		switch request.URL.Path {
		case "/api/machine":
			_ = json.NewEncoder(response).Encode(map[string]string{"machine_id": machineID, "name": machineID})
		case "/api/history":
			_ = json.NewEncoder(response).Encode(map[string]any{"schemaVersion": 1, "sessions": []any{}})
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestFleetPeerLatencyToday measures what a request through the relay costs
// when a saved peer is unreachable or stalling, and in what order the saved
// transports are tried. Run it deliberately; it waits on real timeouts.
//
//	SESSIONS_MEASURE_FLEET=1 go test ./internal/api/ -run FleetPeerLatencyToday -v
func TestFleetPeerLatencyToday(t *testing.T) {
	if os.Getenv("SESSIONS_MEASURE_FLEET") != "1" {
		t.Skip("set SESSIONS_MEASURE_FLEET=1; this waits on dial timeouts")
	}
	for _, test := range []struct {
		name  string
		stall map[string]time.Duration
	}{
		{name: "refusing peer"},
		{name: "stalling peer", stall: map[string]time.Duration{
			"10.129.174.32:8787": 30 * time.Second,
			"100.100.32.1:8787":  30 * time.Second,
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var healthyRequests atomic.Int32
			healthy := healthyPeer(t, "machine-healthy", &healthyRequests)
			dialer := &peerDialer{
				reachAt: map[string]string{healthy.Listener.Addr().String(): healthy.Listener.Addr().String()},
				stall:   test.stall,
			}
			defer dialer.install(t)()

			daemon := newTestDaemon(t)
			savePeers(t, daemon, []fleetSavedMachine{
				{MachineID: "machine-healthy", Name: "Healthy", Endpoint: healthy.URL, LANEndpoint: healthy.URL, Transport: "nearby"},
				{
					MachineID: "machine-away", Name: "Away", Transport: "nearby",
					Endpoint:          "http://10.129.174.32:8787",
					LANEndpoint:       "http://10.129.174.32:8787",
					TailnetEndpoint:   "https://away.example.ts.net",
					TailnetIPEndpoint: "http://100.100.32.1:8787",
				},
			})

			start := time.Now()
			response := serve(t, daemon.handler, http.MethodGet, "/api/fleet/machine-away/api/history", nil, "127.0.0.1:1", nil)
			awayElapsed := time.Since(start)

			start = time.Now()
			healthyResponse := serve(t, daemon.handler, http.MethodGet, "/api/fleet/machine-healthy/api/history", nil, "127.0.0.1:1", nil)
			healthyElapsed := time.Since(start)

			dialer.mu.Lock()
			attempts := append([]peerAttempt(nil), dialer.attempts...)
			dialer.mu.Unlock()
			t.Logf("relay to the away peer: %v (HTTP %d)", awayElapsed.Round(time.Millisecond), response.Code)
			t.Logf("relay to the healthy peer: %v (HTTP %d)", healthyElapsed.Round(time.Millisecond), healthyResponse.Code)
			for _, attempt := range attempts {
				t.Logf("  dial %-28s at %8v  %s", attempt.address, attempt.at.Round(time.Millisecond), attempt.outcome)
			}
			awayAttempts := 0
			for _, attempt := range attempts {
				if strings.Contains(attempt.address, "10.129.174.32") || strings.Contains(attempt.address, "100.100.32.1") ||
					strings.Contains(attempt.address, "away.example.ts.net") {
					awayAttempts++
				}
			}
			t.Logf("  away-peer dial attempts: %d", awayAttempts)
		})
	}
}

// A peer that is off holds the relay for as long as it takes to decide it is
// off. Deciding used to mean one dial timeout per saved address, in turn, so a
// machine with three saved routes held the caller for three of them and a
// fleet-wide read waited behind it. The routes overlap now.
func TestOneAwayPeerDoesNotCostOneTimeoutPerSavedRoute(t *testing.T) {
	const routeStall = 2 * time.Second
	var healthyRequests atomic.Int32
	healthy := healthyPeer(t, "machine-healthy", &healthyRequests)
	dialer := &peerDialer{
		reachAt: map[string]string{healthy.Listener.Addr().String(): healthy.Listener.Addr().String()},
		stall: map[string]time.Duration{
			"10.129.174.32:8787":      routeStall,
			"away.example.ts.net:443": routeStall,
			"100.100.32.1:8787":       routeStall,
		},
	}
	defer dialer.install(t)()

	daemon := newTestDaemon(t)
	savePeers(t, daemon, []fleetSavedMachine{
		{MachineID: "machine-healthy", Name: "Healthy", Endpoint: healthy.URL, LANEndpoint: healthy.URL, Transport: "nearby"},
		{
			MachineID: "machine-away", Name: "Away", Transport: "nearby",
			Endpoint:          "http://10.129.174.32:8787",
			LANEndpoint:       "http://10.129.174.32:8787",
			TailnetEndpoint:   "https://away.example.ts.net",
			TailnetIPEndpoint: "http://100.100.32.1:8787",
		},
	})

	start := time.Now()
	response := serve(t, daemon.handler, http.MethodGet, "/api/fleet/machine-away/api/history", nil, "127.0.0.1:1", nil)
	elapsed := time.Since(start)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("away peer = %d %s", response.Code, response.Body.String())
	}
	// Three routes in turn would be three stalls. Overlapping them costs one,
	// plus the short head start each later route waits out.
	sequential := 3 * routeStall
	if elapsed >= sequential {
		t.Fatalf("deciding one away peer took %v; three routes in turn would be %v", elapsed, sequential)
	}
	if elapsed < routeStall {
		t.Fatalf("deciding one away peer took %v, less than a single route's stall %v; "+
			"the routes were not actually tried", elapsed, routeStall)
	}
	if elapsed > routeStall+2*time.Second {
		t.Fatalf("deciding one away peer took %v, want about one route's stall %v", elapsed, routeStall)
	}
	t.Logf("away peer decided in %v (three routes in turn would be %v)", elapsed.Round(time.Millisecond), sequential)

	// And the healthy peer beside it is unaffected: one dial, no waiting.
	before := healthyRequests.Load()
	start = time.Now()
	healthyResponse := serve(t, daemon.handler, http.MethodGet, "/api/fleet/machine-healthy/api/history", nil, "127.0.0.1:1", nil)
	healthyElapsed := time.Since(start)
	if healthyResponse.Code != http.StatusOK {
		t.Fatalf("healthy peer = %d %s", healthyResponse.Code, healthyResponse.Body.String())
	}
	if healthyElapsed > time.Second {
		t.Fatalf("healthy peer took %v", healthyElapsed)
	}
	if healthyRequests.Load() <= before {
		t.Fatal("healthy peer was not actually reached")
	}
}

// A peer whose preferred route is dead is still reached by a route further down
// the saved order, and the order still decides who wins when more than one
// answers.
func TestAwayFirstRouteStillFallsBackToALiveOne(t *testing.T) {
	var healthyRequests atomic.Int32
	live := healthyPeer(t, "machine-mixed", &healthyRequests)
	dialer := &peerDialer{
		reachAt: map[string]string{"100.100.32.1:8787": live.Listener.Addr().String()},
		stall:   map[string]time.Duration{"10.129.174.32:8787": 2 * time.Second},
	}
	defer dialer.install(t)()

	daemon := newTestDaemon(t)
	savePeers(t, daemon, []fleetSavedMachine{{
		MachineID: "machine-mixed", Name: "Mixed", Transport: "nearby",
		Endpoint:          "http://10.129.174.32:8787",
		LANEndpoint:       "http://10.129.174.32:8787",
		TailnetIPEndpoint: "http://100.100.32.1:8787",
	}})

	response := serve(t, daemon.handler, http.MethodGet, "/api/fleet/machines", nil, "127.0.0.1:1", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("fleet listing = %d %s", response.Code, response.Body.String())
	}
	var body struct {
		Machines []fleetMachineView `json:"machines"`
	}
	decodeBody(t, response, &body)
	if len(body.Machines) != 1 || !body.Machines[0].Reachable || body.Machines[0].Transport != "tailnet-ip" {
		t.Fatalf("mixed peer = %+v", body.Machines)
	}
}
