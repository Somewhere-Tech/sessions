package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/fleetendpoint"
)

func TestPairWithoutReachableEndpointReportsDaemonTeaching(t *testing.T) {
	ticketRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/api/pair/ticket":
			ticketRequests++
			response.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(response, `{"error":"pairing needs a LAN or Tailscale endpoint; enable one in Settings > Fleet, then try again"}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "pair"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Fatalf("pair unexpectedly succeeded: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if ticketRequests != 1 {
		t.Fatalf("ticket requests = %d, want one daemon-owned endpoint check", ticketRequests)
	}
	for _, teaching := range []string{"LAN or Tailscale endpoint", "Settings > Fleet"} {
		if !strings.Contains(stderr.String(), teaching) {
			t.Fatalf("teaching error missing %q: %q", teaching, stderr.String())
		}
	}
}

func TestPairUsesVerifiedLANAndPrintsOneQR(t *testing.T) {
	requestedName := ""
	requestedTTL := ""
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/api/pair/ticket":
			var body struct {
				Name string `json:"name"`
				TTL  string `json:"ttl"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode ticket request: %v", err)
			}
			requestedName = body.Name
			requestedTTL = body.TTL
			endpoint := serverURL(request)
			link, _ := fleetendpoint.PairingLink(
				[]fleetendpoint.Candidate{{Endpoint: endpoint, Transport: "lan"}},
				"ticket-id.ticket-secret",
			)
			_ = json.NewEncoder(response).Encode(pairTicketResponse{
				Ticket: "ticket-id.ticket-secret", TicketID: "ticket-id",
				ExpiresAt: time.Date(2026, 7, 19, 12, 10, 0, 0, time.UTC),
				Link:      link, Fallback: endpoint + "/pair/ticket-id.ticket-secret",
				Endpoints: []fleetendpoint.Candidate{{Endpoint: endpoint, Transport: "lan"}},
			})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	previousPicker := primaryLANIPv4
	primaryLANIPv4 = func() (net.IP, error) { return net.ParseIP("127.0.0.1"), nil }
	t.Cleanup(func() { primaryLANIPv4 = previousPicker })

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "pair", "--ttl", "10m", "--name", "Pocket browser"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("pair exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if requestedName != "Pocket browser" || requestedTTL != "10m0s" {
		t.Fatalf("ticket request name=%q ttl=%q", requestedName, requestedTTL)
	}
	if count := strings.Count(stdout.String(), "Scan on your phone:"); count != 1 {
		t.Fatalf("QR headings = %d, want one: %q", count, stdout.String())
	}
	for _, expected := range []string{
		"sessions://pair?",
		server.URL + "/pair/ticket-id.ticket-secret",
		"10m0s; single use",
		"Revoke this unused link from Settings > Fleet",
	} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("pair output missing %q: %q", expected, stdout.String())
		}
	}
}

func TestDevicesListJSONAndRevokePrefix(t *testing.T) {
	created := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	devices := []pairedDevice{
		{DeviceID: "11111111-1111-4111-8111-111111111111", Name: "Phone", CreatedAt: created, LastUsedAt: created.Add(time.Hour)},
		{DeviceID: "22222222-2222-4222-8222-222222222222", Name: "Tablet", CreatedAt: created.Add(time.Hour), LastUsedAt: created.Add(2 * time.Hour)},
	}
	revokedID := ""
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/devices":
			_ = json.NewEncoder(response).Encode(pairedDevicesResponse{Devices: devices})
		case request.Method == http.MethodDelete && strings.HasPrefix(request.URL.Path, "/api/devices/"):
			revokedID = strings.TrimPrefix(request.URL.Path, "/api/devices/")
			_, _ = io.WriteString(response, `{"ok":true}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "devices"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("list exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, expected := range []string{"ID", "NAME", "CREATED", "LAST USED", "11111111", "Phone", "22222222", "Tablet"} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("device table missing %q: %q", expected, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"--host", server.URL, "devices", "revoke", "11111111"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 || revokedID != devices[0].DeviceID {
		t.Fatalf("revoke exit=%d revoked=%q stdout=%q stderr=%q", code, revokedID, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Revoked Phone ("+devices[0].DeviceID+")") {
		t.Fatalf("revoke confirmation = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"--host", server.URL, "--json", "devices"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("json list exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var parsed pairedDevicesResponse
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil || len(parsed.Devices) != 2 {
		t.Fatalf("json list = %#v, err=%v, output=%q", parsed, err, stdout.String())
	}
}

func TestAccessAcceptResolvesThePrintedRequestPrefix(t *testing.T) {
	requestID := "534c15f6-1111-4111-8111-111111111111"
	pending := accessRequest{RequestID: requestID, Name: "Laptop", Transport: "tailnet"}
	decidedID := ""
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/access/requests":
			_ = json.NewEncoder(response).Encode(accessRequestsResponse{Requests: []accessRequest{pending}})
		case request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/api/access/requests/"):
			decidedID = strings.TrimPrefix(request.URL.Path, "/api/access/requests/")
			_ = json.NewEncoder(response).Encode(pending)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"--host", server.URL, "access", "accept", requestID[:8]},
		strings.NewReader(""), &stdout, &stderr,
	)
	if code != 0 || stderr.Len() != 0 || decidedID != requestID {
		t.Fatalf("accept exit=%d decided=%q stdout=%q stderr=%q", code, decidedID, stdout.String(), stderr.String())
	}
}
