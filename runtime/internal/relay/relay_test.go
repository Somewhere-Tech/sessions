package relay

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/somewhere-tech/sessions/runtime/internal/relayauth"
)

func TestRelayMultiplexesTwoDaemonsAndPreservesDeviceAuth(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	allow := map[string]string{}
	keys := map[string]ed25519.PrivateKey{}
	for _, id := range []string{"machine-a", "machine-b"} {
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		allow[id] = base64.RawURLEncoding.EncodeToString(public)
		keys[id] = private
	}
	allowPath := writeAllowList(t, allow)
	relayServer := httptest.NewServer(NewServer(ServerOptions{Authorizer: AllowListAuthorizer{Path: allowPath}}))
	defer relayServer.Close()

	daemons := make([]*httptest.Server, 0, 2)
	for _, id := range []string{"machine-a", "machine-b"} {
		daemon := httptest.NewServer(authenticatedTestDaemon(id, "token-"+id))
		daemons = append(daemons, daemon)
		connector := testConnector(t, relayServer.URL, daemon.URL, id, allow[id], keys[id])
		go func() { _ = connector.RunOnce(ctx) }()
	}
	defer func() {
		for _, daemon := range daemons {
			daemon.Close()
		}
	}()
	waitForMachines(t, relayServer.URL, 2)

	for _, id := range []string{"machine-a", "machine-b"} {
		request, _ := http.NewRequest(http.MethodGet, relayServer.URL+"/m/"+id+"/api/machine", nil)
		request.Header.Set("Authorization", "Bearer token-"+id)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]string
		_ = json.NewDecoder(response.Body).Decode(&body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || body["machine_id"] != id {
			t.Fatalf("%s response = %d %#v", id, response.StatusCode, body)
		}
	}

	bad, _ := http.Get(relayServer.URL + "/m/machine-a/api/machine")
	bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("request without device credential = %d, want 401", bad.StatusCode)
	}
	testConcurrentStreams(t, relayServer.URL)
	testRelayWebSocket(t, relayServer.URL)
}

func testConcurrentStreams(t *testing.T, relayURL string) {
	t.Helper()
	var wait sync.WaitGroup
	errors := make(chan error, 12)
	for range 12 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			request, _ := http.NewRequest(http.MethodGet, relayURL+"/m/machine-a/api/machine", nil)
			request.Header.Set("Authorization", "Bearer token-machine-a")
			response, err := http.DefaultClient.Do(request)
			if err == nil {
				response.Body.Close()
				if response.StatusCode != http.StatusOK {
					err = fmt.Errorf("concurrent relay request returned HTTP %d", response.StatusCode)
				}
			}
			if err != nil {
				errors <- err
			}
		}()
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
}

func TestCloneRequestStripsProxyIdentity(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "https://relay.example/m/machine-a/api/machine", nil)
	request.RemoteAddr = "203.0.113.8:1234"
	request.Header.Set("Authorization", "Bearer device-token")
	request.Header.Set("Tailscale-User-Login", "attacker@example.com")
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-Sessions-Relay-Forwarded", "forged")

	cloned := cloneRequest(request, "/api/machine")
	if got := cloned.Header.Get("Authorization"); got != "Bearer device-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := cloned.Header.Get("Tailscale-User-Login"); got != "" {
		t.Fatalf("Tailscale-User-Login = %q, want stripped", got)
	}
	if got := cloned.Header.Get("X-Forwarded-Proto"); got != "" {
		t.Fatalf("X-Forwarded-Proto = %q, want stripped", got)
	}
	if got := cloned.Header.Get("X-Forwarded-For"); got != "203.0.113.8" {
		t.Fatalf("X-Forwarded-For = %q", got)
	}
	if got := cloned.Header.Get("X-Sessions-Relay-Forwarded"); got != "1" {
		t.Fatalf("X-Sessions-Relay-Forwarded = %q", got)
	}
}

func TestConnectorEnforcesRemoteAuthenticationBoundary(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/machine", nil)
	request.Header.Set("Tailscale-User-Login", "attacker@example.com")
	request.Header.Set("X-Forwarded-For", "127.0.0.1")
	secureForwardedRequest(request)
	if got := request.Header.Get("Tailscale-User-Login"); got != "" {
		t.Fatalf("Tailscale-User-Login = %q, want stripped", got)
	}
	if got := request.Header.Get("X-Forwarded-For"); got != "sessions-relay" {
		t.Fatalf("X-Forwarded-For = %q", got)
	}
	if got := request.Header.Get("X-Sessions-Relay-Forwarded"); got != "1" {
		t.Fatalf("X-Sessions-Relay-Forwarded = %q", got)
	}
}

func TestRelayFramePayloadLimit(t *testing.T) {
	if _, err := encodeFrame(frameData, 1, make([]byte, MaxPayload)); err != nil {
		t.Fatalf("encode maximum frame: %v", err)
	}
	if _, err := encodeFrame(frameData, 1, make([]byte, MaxPayload+1)); err == nil {
		t.Fatal("encode frame larger than 64 KiB succeeded")
	}
	malformed, _ := encodeFrame(frameOpen, 1, []byte("unexpected"))
	if _, err := decodeFrame(malformed); err == nil {
		t.Fatal("control frame with a payload decoded")
	}
}

func authenticatedTestDaemon(machineID, token string) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		provided := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		if provided == "" {
			provided = request.URL.Query().Get("token")
		}
		if provided != token {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		if request.URL.Path == "/ws" {
			connection, err := websocket.Accept(response, request, nil)
			if err != nil {
				return
			}
			defer connection.Close(websocket.StatusNormalClosure, "done")
			messageType, data, err := connection.Read(request.Context())
			if err == nil {
				_ = connection.Write(request.Context(), messageType, data)
			}
			return
		}
		_ = json.NewEncoder(response).Encode(map[string]string{"machine_id": machineID})
	})
}

func testConnector(t *testing.T, relayURL, daemonURL, id, public string, private ed25519.PrivateKey) *Connector {
	t.Helper()
	target, _ := url.Parse(daemonURL)
	return NewConnector(ConnectorOptions{
		URL: func(context.Context) (string, error) { return relayURL, nil }, Target: target.Host,
		Authenticate: func(challenge relayauth.Challenge) (relayauth.Response, error) {
			return relayauth.Response{
				MachineID: id, PublicKey: public,
				Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, relayauth.Canonical(id, challenge))),
			}, nil
		},
	})
}

func writeAllowList(t *testing.T, machines map[string]string) string {
	t.Helper()
	path := t.TempDir() + "/allow.json"
	encoded, _ := json.Marshal(map[string]any{"machines": machines})
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitForMachines(t *testing.T, relayURL string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(relayURL + "/healthz")
		if err == nil {
			var health struct {
				Machines int `json:"machines"`
			}
			_ = json.NewDecoder(response.Body).Decode(&health)
			response.Body.Close()
			if health.Machines == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("relay did not report %d machines", want)
}

func testRelayWebSocket(t *testing.T, relayURL string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(relayURL, "http") + "/m/machine-a/ws?token=token-machine-a"
	connection, response, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("dial relay websocket (HTTP %d): %v", status, err)
	}
	defer connection.Close(websocket.StatusNormalClosure, "done")
	message := []byte(`{"type":"ping"}`)
	if err := connection.Write(ctx, websocket.MessageText, message); err != nil {
		t.Fatal(err)
	}
	_, echoed, err := connection.Read(ctx)
	if err != nil || string(echoed) != string(message) {
		t.Fatalf("websocket echo = %q, %v", echoed, err)
	}
}
