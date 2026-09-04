package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/discovery"
	"github.com/somewhere-tech/sessions/runtime/internal/localnetwork"
)

func TestLANDiscoverRunsInDaemonAndReturnsVerifiedPeers(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/health" {
			http.NotFound(response, request)
			return
		}
		_ = json.NewEncoder(response).Encode(map[string]any{
			"ok": true, "name": "sessionsd", "version": "v0.2.27", "sessionsLoaded": 4,
			"system": map[string]string{"os": "darwin", "arch": "arm64"},
		})
	}))
	defer peer.Close()
	daemon := newTestDaemon(t)
	daemon.handler.lan.browse = func(context.Context, time.Duration) ([]discovery.Candidate, error) {
		return []discovery.Candidate{{Name: "Mini", Endpoint: peer.URL, Transport: "nearby"}}, nil
	}
	response := serve(t, daemon.handler, http.MethodGet, "/api/lan/discover?timeout=10ms", nil, "127.0.0.1:1", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"sessions_loaded":4`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestLANDiscoverExplainsEmptyBrowseWhileAdvertising(t *testing.T) {
	daemon := newTestDaemon(t)
	daemon.handler.lan.browse = func(context.Context, time.Duration) ([]discovery.Candidate, error) { return nil, nil }
	daemon.handler.lan.mu.Lock()
	daemon.handler.lan.server = &http.Server{}
	daemon.handler.lan.url = "http://10.0.0.1:8787"
	daemon.handler.lan.registration = &fakeBonjourRegistration{}
	daemon.handler.lan.mu.Unlock()
	response := serve(t, daemon.handler, http.MethodGet, "/api/lan/discover?timeout=10ms", nil, "127.0.0.1:1", nil)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "macOS has not allowed Sessions") ||
		!strings.Contains(response.Body.String(), localnetwork.Reason) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestLANConnectOwnsPeerDialAndReturnsIssuedCredential(t *testing.T) {
	paths := make([]string, 0, 3)
	peer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		switch request.URL.Path {
		case "/api/lan/access/request":
			response.WriteHeader(http.StatusAccepted)
			_, _ = response.Write([]byte(`{"request_id":"request-id","request_secret":"secret"}`))
		case "/api/lan/access/claim":
			response.WriteHeader(http.StatusCreated)
			_, _ = response.Write([]byte(`{"device_id":"device-id","token":"device-token","machine_id":"machine-mini","machine_name":"Mini"}`))
		case "/api/machine":
			_ = json.NewEncoder(response).Encode(map[string]string{"machine_id": "machine-mini"})
		default:
			http.NotFound(response, request)
		}
	}))
	defer peer.Close()
	daemon := newTestDaemon(t)
	body := `{"endpoint":"` + peer.URL + `","client_id":"11111111-1111-4111-8111-111111111111","name":"MacBook","timeout":"1s"}`
	response := serve(t, daemon.handler, http.MethodPost, "/api/lan/connect", strings.NewReader(body), "127.0.0.1:1", nil)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"device-token"`) {
		t.Fatalf("status=%d body=%s paths=%v", response.Code, response.Body.String(), paths)
	}
	want := []string{"/api/lan/access/request", "/api/lan/access/claim", "/api/machine"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
}
