package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

const externalPairingPeer = "198.51.100.42:4242"

func TestPairTicketSingleUseExpiryAndUnknown(t *testing.T) {
	t.Run("single use", func(t *testing.T) {
		daemon := newTestDaemon(t)
		ticket := mintPairingTicket(t, daemon.handler, "Phone")
		claimed := claimPairingTicket(t, daemon.handler, ticket.Ticket, "", http.StatusCreated)
		if claimed.Name != "Phone" || claimed.DeviceID == "" || claimed.Token == "" ||
			!validMachineID(claimed.MachineID) || claimed.MachineName == "" {
			t.Fatalf("claim response = %#v", claimed)
		}
		response := claimPairingTicketResponse(t, daemon.handler, ticket.Ticket, "", http.StatusGone)
		if !strings.Contains(response.Body.String(), "already used") || !strings.Contains(response.Body.String(), "sessions pair") {
			t.Fatalf("second claim teaching error = %q", response.Body.String())
		}
	})

	t.Run("expired", func(t *testing.T) {
		daemon := newTestDaemon(t)
		now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
		daemon.handler.pair.setNow(func() time.Time { return now })
		ticket := mintPairingTicket(t, daemon.handler, "")
		now = now.Add(pairTicketTTL + time.Second)
		response := claimPairingTicketResponse(t, daemon.handler, ticket.Ticket, "", http.StatusGone)
		if !strings.Contains(response.Body.String(), "expired") {
			t.Fatalf("expired claim body = %q", response.Body.String())
		}
	})

	t.Run("unknown", func(t *testing.T) {
		daemon := newTestDaemon(t)
		response := claimPairingTicketResponse(t, daemon.handler, "unknown-id.unknown-secret", "", http.StatusGone)
		if !strings.Contains(response.Body.String(), "invalid") {
			t.Fatalf("unknown claim body = %q", response.Body.String())
		}
	})
}

func TestPairTicketSurvivesDevicePersistenceFailure(t *testing.T) {
	root := t.TempDir()
	service := newPairService(state.Config{StateRoot: root, UserStateRoot: root})
	ticket, err := service.mint("MacBook")
	if err != nil {
		t.Fatal(err)
	}

	unwritableTarget := filepath.Join(root, "devices-target-is-a-directory")
	if err := os.MkdirAll(unwritableTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	service.devices.path = unwritableTarget
	if _, err := service.claim(ticket.Ticket, "", "test"); err == nil {
		t.Fatal("claim unexpectedly succeeded when the device credential could not be persisted")
	}

	service.devices.path = filepath.Join(root, "devices.json")
	claimed, err := service.claim(ticket.Ticket, "", "test")
	if err != nil {
		t.Fatalf("retry after persistence failure: %v", err)
	}
	if claimed.DeviceID == "" || claimed.Token == "" {
		t.Fatalf("retry claim = %#v", claimed)
	}
}

func TestDeviceTokenMasterTokenAndLoopbackAuthorization(t *testing.T) {
	daemon := newTestDaemon(t)
	ticket := mintPairingTicket(t, daemon.handler, "")
	headers := http.Header{"User-Agent": {strings.Repeat("Phone Browser ", 10)}}
	claimed := claimPairingTicketWithHeaders(t, daemon.handler, ticket.Ticket, "", http.StatusCreated, headers)
	if len([]rune(claimed.Name)) != maximumPairingDeviceName {
		t.Fatalf("default User-Agent name length = %d, want %d: %q", len([]rune(claimed.Name)), maximumPairingDeviceName, claimed.Name)
	}

	deviceHeaders := http.Header{"Authorization": {"Bearer " + claimed.Token}}
	device := serve(t, daemon.handler, http.MethodGet, "/api/sessions", nil, externalPairingPeer, deviceHeaders)
	if device.Code != http.StatusOK {
		t.Fatalf("device token status = %d, body=%s", device.Code, device.Body.String())
	}
	deviceQuery := serve(t, daemon.handler, http.MethodGet, "/api/sessions?token="+url.QueryEscape(claimed.Token), nil, externalPairingPeer, nil)
	if deviceQuery.Code != http.StatusOK {
		t.Fatalf("device query token status = %d, body=%s", deviceQuery.Code, deviceQuery.Body.String())
	}
	masterHeaders := http.Header{"Authorization": {"Bearer " + testToken}}
	master := serve(t, daemon.handler, http.MethodGet, "/api/sessions", nil, externalPairingPeer, masterHeaders)
	if master.Code != http.StatusOK {
		t.Fatalf("master token status = %d, body=%s", master.Code, master.Body.String())
	}
	loopback := serve(t, daemon.handler, http.MethodGet, "/api/sessions", nil, "127.0.0.1:1234", nil)
	if loopback.Code != http.StatusOK {
		t.Fatalf("loopback status = %d, body=%s", loopback.Code, loopback.Body.String())
	}

	devicesPath := filepath.Join(daemon.config.StateRoot, "devices.json")
	encoded, err := os.ReadFile(devicesPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(claimed.Token)) {
		t.Fatalf("devices file contains plaintext token: %s", encoded)
	}
	assertMode(t, devicesPath, 0o600)
}

func TestDeviceRevocationAndPersistenceRoundTrip(t *testing.T) {
	daemon := newTestDaemon(t)
	claimed := claimPairingTicket(t, daemon.handler, mintPairingTicket(t, daemon.handler, "Tablet").Ticket, "", http.StatusCreated)
	deviceHeaders := http.Header{"Authorization": {"Bearer " + claimed.Token}}

	restarted := New(daemon.config, daemon.registry)
	before := serve(t, restarted, http.MethodGet, "/api/devices", nil, externalPairingPeer, deviceHeaders)
	if before.Code != http.StatusOK {
		t.Fatalf("restarted device authorization status = %d, body=%s", before.Code, before.Body.String())
	}
	var listed struct {
		Devices []deviceView `json:"devices"`
	}
	decodeBody(t, before, &listed)
	if len(listed.Devices) != 1 || listed.Devices[0].DeviceID != claimed.DeviceID || listed.Devices[0].Name != "Tablet" {
		t.Fatalf("restarted device list = %#v", listed.Devices)
	}

	masterHeaders := http.Header{"Authorization": {"Bearer " + testToken}}
	revoked := serve(t, restarted, http.MethodDelete, "/api/devices/"+claimed.DeviceID, nil, externalPairingPeer, masterHeaders)
	if revoked.Code != http.StatusOK {
		t.Fatalf("revoke status = %d, body=%s", revoked.Code, revoked.Body.String())
	}
	after := serve(t, restarted, http.MethodGet, "/api/sessions", nil, externalPairingPeer, deviceHeaders)
	if after.Code != http.StatusUnauthorized {
		t.Fatalf("revoked device token status = %d, body=%s", after.Code, after.Body.String())
	}

	restartedAgain := New(daemon.config, daemon.registry)
	listedAfter := serve(t, restartedAgain, http.MethodGet, "/api/devices", nil, externalPairingPeer, masterHeaders)
	decodeBody(t, listedAfter, &listed)
	if len(listed.Devices) != 0 {
		t.Fatalf("devices after persisted revoke = %#v", listed.Devices)
	}
}

func TestPairClaimRateLimiter(t *testing.T) {
	daemon := newTestDaemon(t)
	for attempt := 0; attempt < pairFailureLimit; attempt++ {
		response := claimPairingTicketResponse(t, daemon.handler, "bad-ticket", "", http.StatusGone)
		if response.Code != http.StatusGone {
			t.Fatalf("attempt %d status = %d", attempt+1, response.Code)
		}
	}
	limited := claimPairingTicketResponse(t, daemon.handler, "bad-ticket", "", http.StatusTooManyRequests)
	if !strings.Contains(limited.Body.String(), "Wait one minute") {
		t.Fatalf("rate-limit body = %q", limited.Body.String())
	}
}

func mintPairingTicket(t *testing.T, handler *Server, name string) pairingTicketResponse {
	t.Helper()
	encoded, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		t.Fatal(err)
	}
	response := serve(t, handler, http.MethodPost, "/api/pair/ticket", bytes.NewReader(encoded), "127.0.0.1:1234", nil)
	if response.Code != http.StatusCreated {
		t.Fatalf("mint status = %d, body=%s", response.Code, response.Body.String())
	}
	var ticket pairingTicketResponse
	decodeBody(t, response, &ticket)
	if ticket.Ticket == "" || ticket.ExpiresAt.IsZero() {
		t.Fatalf("mint response = %#v", ticket)
	}
	return ticket
}

func claimPairingTicket(t *testing.T, handler *Server, ticket, name string, wantStatus int) pairingClaimResponse {
	t.Helper()
	return claimPairingTicketWithHeaders(t, handler, ticket, name, wantStatus, nil)
}

func claimPairingTicketWithHeaders(t *testing.T, handler *Server, ticket, name string, wantStatus int, headers http.Header) pairingClaimResponse {
	t.Helper()
	response := claimPairingTicketResponseWithHeaders(t, handler, ticket, name, wantStatus, headers)
	var claimed pairingClaimResponse
	decodeBody(t, response, &claimed)
	return claimed
}

func claimPairingTicketResponse(t *testing.T, handler *Server, ticket, name string, wantStatus int) *httptest.ResponseRecorder {
	t.Helper()
	return claimPairingTicketResponseWithHeaders(t, handler, ticket, name, wantStatus, nil)
}

func claimPairingTicketResponseWithHeaders(t *testing.T, handler *Server, ticket, name string, wantStatus int, headers http.Header) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(map[string]string{"ticket": ticket, "name": name})
	if err != nil {
		t.Fatal(err)
	}
	response := serve(t, handler, http.MethodPost, "/api/pair/claim", bytes.NewReader(encoded), externalPairingPeer, headers)
	if response.Code != wantStatus {
		t.Fatalf("claim status = %d, want %d, body=%s", response.Code, wantStatus, response.Body.String())
	}
	return response
}
