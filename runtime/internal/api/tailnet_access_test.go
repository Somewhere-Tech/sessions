package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func serveNearby(
	t *testing.T,
	handler http.Handler,
	method, target string,
	body *strings.Reader,
	remote string,
	headers http.Header,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, body)
	request.Host = "127.0.0.1:8787"
	if server, ok := handler.(*Server); ok {
		request.Host = net.JoinHostPort(server.config.Host, strconv.Itoa(server.config.Port))
	}
	request.RemoteAddr = remote
	request = request.WithContext(context.WithValue(request.Context(), lanRequestContextKey{}, true))
	for key, values := range headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func tailnetHeaders(login, name string) http.Header {
	return http.Header{
		"X-Forwarded-For":      {"100.100.100.10"},
		"Tailscale-User-Login": {login},
		"Tailscale-User-Name":  {name},
		"Content-Type":         {"application/json"},
	}
}

func TestTailnetAccessRequiresServeIdentity(t *testing.T) {
	daemon := newTestDaemon(t)
	body := strings.NewReader(`{"client_id":"11111111-1111-4111-8111-111111111111","name":"MacBook"}`)

	missing := serve(t, daemon.handler, http.MethodPost, "/api/tailnet/access/request", body, "127.0.0.1:4040", nil)
	if missing.Code != http.StatusForbidden {
		t.Fatalf("missing identity status = %d, body=%s", missing.Code, missing.Body.String())
	}
	spoofed := serve(
		t, daemon.handler, http.MethodPost, "/api/tailnet/access/request",
		strings.NewReader(`{"client_id":"11111111-1111-4111-8111-111111111111","name":"MacBook"}`),
		"192.168.1.20:4040", tailnetHeaders("uzair@example.com", "Uzair"),
	)
	if spoofed.Code != http.StatusForbidden {
		t.Fatalf("direct spoof status = %d, body=%s", spoofed.Code, spoofed.Body.String())
	}

	for _, origin := range []string{
		"https://sessions.somewhere.tech",
		"https://mac-mini.example.ts.net",
		"https://evil.example",
	} {
		headers := tailnetHeaders("uzair@example.com", "Uzair")
		headers.Set("Origin", origin)
		rejected := serve(
			t, daemon.handler, http.MethodPost, "/api/tailnet/access/request",
			strings.NewReader(`{"client_id":"11111111-1111-4111-8111-111111111111","name":"MacBook"}`),
			"127.0.0.1:4040", headers,
		)
		if rejected.Code != http.StatusForbidden {
			t.Fatalf("browser origin %q status = %d, body=%s", origin, rejected.Code, rejected.Body.String())
		}
	}
	simpleHeaders := tailnetHeaders("uzair@example.com", "Uzair")
	simpleHeaders.Set("Content-Type", "text/plain")
	simple := serve(
		t, daemon.handler, http.MethodPost, "/api/tailnet/access/request",
		strings.NewReader(`{"client_id":"11111111-1111-4111-8111-111111111111","name":"MacBook"}`),
		"127.0.0.1:4040", simpleHeaders,
	)
	if simple.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("simple POST status = %d, body=%s", simple.Code, simple.Body.String())
	}
}

func TestTailnetAccessRequestAcceptAndClaim(t *testing.T) {
	daemon := newTestDaemon(t)
	identity := tailnetHeaders("uzair@example.com", "Uzair Haq")
	requestBody := strings.NewReader(`{"client_id":"11111111-1111-4111-8111-111111111111","name":"MacBook Pro"}`)
	created := serve(t, daemon.handler, http.MethodPost, "/api/tailnet/access/request", requestBody, "127.0.0.1:4040", identity)
	if created.Code != http.StatusAccepted {
		t.Fatalf("request status = %d, body=%s", created.Code, created.Body.String())
	}
	var request tailnetAccessRequestResponse
	decodeBody(t, created, &request)
	if !validMachineID(request.RequestID) || request.RequestSecret == "" || request.Status != "pending" {
		t.Fatalf("request response = %#v", request)
	}
	remoteAdmin := serve(
		t, daemon.handler, http.MethodGet, "/api/tailnet/access/requests", nil, "198.51.100.20:6060",
		http.Header{"Authorization": {"Bearer " + testToken}},
	)
	if remoteAdmin.Code != http.StatusForbidden {
		t.Fatalf("remote access administration status = %d, body=%s", remoteAdmin.Code, remoteAdmin.Body.String())
	}

	duplicate := serve(
		t, daemon.handler, http.MethodPost, "/api/tailnet/access/request",
		strings.NewReader(`{"client_id":"11111111-1111-4111-8111-111111111111","name":"Renamed"}`),
		"127.0.0.1:4040", identity,
	)
	var duplicateRequest tailnetAccessRequestResponse
	decodeBody(t, duplicate, &duplicateRequest)
	if duplicateRequest.RequestID != request.RequestID || duplicateRequest.RequestSecret != request.RequestSecret {
		t.Fatalf("duplicate request was not stable: first=%#v duplicate=%#v", request, duplicateRequest)
	}

	listed := serve(t, daemon.handler, http.MethodGet, "/api/tailnet/access/requests", nil, "127.0.0.1:5050", nil)
	var list struct {
		Requests []tailnetAccessRequestView `json:"requests"`
	}
	decodeBody(t, listed, &list)
	if len(list.Requests) != 1 ||
		list.Requests[0].Name != "MacBook Pro" ||
		list.Requests[0].Login != "uzair@example.com" {
		t.Fatalf("pending requests = %#v", list.Requests)
	}

	claimBody, _ := json.Marshal(map[string]string{
		"request_id": request.RequestID, "request_secret": request.RequestSecret,
	})
	pending := serve(t, daemon.handler, http.MethodPost, "/api/tailnet/access/claim", bytes.NewReader(claimBody), "127.0.0.1:4040", identity)
	if pending.Code != http.StatusAccepted || !strings.Contains(pending.Body.String(), `"pending"`) {
		t.Fatalf("pending claim status = %d, body=%s", pending.Code, pending.Body.String())
	}

	decision := serve(
		t, daemon.handler, http.MethodPost, "/api/tailnet/access/requests/"+request.RequestID,
		strings.NewReader(`{"decision":"accept"}`), "127.0.0.1:5050",
		http.Header{"Content-Type": {"application/json"}},
	)
	if decision.Code != http.StatusOK || !strings.Contains(decision.Body.String(), `"accepted"`) {
		t.Fatalf("accept status = %d, body=%s", decision.Code, decision.Body.String())
	}

	wrongIdentity := tailnetHeaders("someone-else@example.com", "Someone Else")
	wrong := serve(t, daemon.handler, http.MethodPost, "/api/tailnet/access/claim", bytes.NewReader(claimBody), "127.0.0.1:4040", wrongIdentity)
	if wrong.Code != http.StatusGone {
		t.Fatalf("wrong identity claim status = %d, body=%s", wrong.Code, wrong.Body.String())
	}

	claimed := serve(t, daemon.handler, http.MethodPost, "/api/tailnet/access/claim", bytes.NewReader(claimBody), "127.0.0.1:4040", identity)
	if claimed.Code != http.StatusCreated {
		t.Fatalf("accepted claim status = %d, body=%s", claimed.Code, claimed.Body.String())
	}
	var credential pairingClaimResponse
	decodeBody(t, claimed, &credential)
	if credential.Name != "MacBook Pro" ||
		credential.Token == "" ||
		!validMachineID(credential.DeviceID) ||
		!validMachineID(credential.MachineID) {
		t.Fatalf("credential = %#v", credential)
	}

	authorized := serve(
		t, daemon.handler, http.MethodGet, "/api/sessions", nil, "198.51.100.20:6060",
		http.Header{"Authorization": {"Bearer " + credential.Token}},
	)
	if authorized.Code != http.StatusOK {
		t.Fatalf("issued device token status = %d, body=%s", authorized.Code, authorized.Body.String())
	}
}

func TestNearbyAccessRequestAcceptAndClaim(t *testing.T) {
	daemon := newTestDaemon(t)
	headers := http.Header{"Content-Type": {"application/json"}}
	created := serveNearby(
		t, daemon.handler, http.MethodPost, "/api/lan/access/request",
		strings.NewReader(`{"client_id":"55555555-5555-4555-8555-555555555555","name":"Nearby Mac"}`),
		"192.168.50.20:4040", headers,
	)
	if created.Code != http.StatusAccepted {
		t.Fatalf("request status = %d, body=%s", created.Code, created.Body.String())
	}
	var pendingRequest tailnetAccessRequestResponse
	decodeBody(t, created, &pendingRequest)

	listed := serve(t, daemon.handler, http.MethodGet, "/api/access/requests", nil, "127.0.0.1:5050", nil)
	var list struct {
		Requests []tailnetAccessRequestView `json:"requests"`
	}
	decodeBody(t, listed, &list)
	if len(list.Requests) != 1 ||
		list.Requests[0].Transport != "nearby" ||
		list.Requests[0].Address != "192.168.50.20" ||
		list.Requests[0].Login != "nearby:192.168.50.20" {
		t.Fatalf("nearby request view = %#v", list.Requests)
	}

	decision := serve(
		t, daemon.handler, http.MethodPost, "/api/access/requests/"+pendingRequest.RequestID,
		strings.NewReader(`{"decision":"accept"}`), "127.0.0.1:5050", headers,
	)
	if decision.Code != http.StatusOK {
		t.Fatalf("accept status = %d, body=%s", decision.Code, decision.Body.String())
	}
	claim := serveNearby(
		t, daemon.handler, http.MethodPost, "/api/lan/access/claim",
		strings.NewReader(`{"request_id":"`+pendingRequest.RequestID+`","request_secret":"`+pendingRequest.RequestSecret+`"}`),
		"192.168.50.20:4041", headers,
	)
	if claim.Code != http.StatusCreated {
		t.Fatalf("claim status = %d, body=%s", claim.Code, claim.Body.String())
	}
	var credential pairingClaimResponse
	decodeBody(t, claim, &credential)
	if credential.Token == "" || !validMachineID(credential.MachineID) {
		t.Fatalf("credential = %#v", credential)
	}
}

func TestNearbyAccessRejectsUntrustedSurfaces(t *testing.T) {
	daemon := newTestDaemon(t)
	payload := `{"client_id":"66666666-6666-4666-8666-666666666666","name":"Unknown"}`
	headers := http.Header{"Content-Type": {"application/json"}}
	direct := serve(
		t, daemon.handler, http.MethodPost, "/api/lan/access/request",
		strings.NewReader(payload), "192.168.50.20:4040", headers,
	)
	if direct.Code != http.StatusForbidden {
		t.Fatalf("main listener status = %d, body=%s", direct.Code, direct.Body.String())
	}
	loopback := serveNearby(
		t, daemon.handler, http.MethodPost, "/api/lan/access/request",
		strings.NewReader(payload), "127.0.0.1:4040", headers,
	)
	if loopback.Code != http.StatusForbidden {
		t.Fatalf("loopback status = %d, body=%s", loopback.Code, loopback.Body.String())
	}
	browserHeaders := headers.Clone()
	browserHeaders.Set("Origin", "http://192.168.50.10:8787")
	browser := serveNearby(
		t, daemon.handler, http.MethodPost, "/api/lan/access/request",
		strings.NewReader(payload), "192.168.50.20:4040", browserHeaders,
	)
	if browser.Code != http.StatusForbidden {
		t.Fatalf("browser status = %d, body=%s", browser.Code, browser.Body.String())
	}
}

func TestTailnetAccessDeny(t *testing.T) {
	daemon := newTestDaemon(t)
	identity := tailnetHeaders("uzair@example.com", "Uzair Haq")
	created := serve(
		t, daemon.handler, http.MethodPost, "/api/tailnet/access/request",
		strings.NewReader(`{"client_id":"22222222-2222-4222-8222-222222222222","name":"Unknown Mac"}`),
		"127.0.0.1:4040", identity,
	)
	var request tailnetAccessRequestResponse
	decodeBody(t, created, &request)
	denied := serve(
		t, daemon.handler, http.MethodPost, "/api/tailnet/access/requests/"+request.RequestID,
		strings.NewReader(`{"decision":"deny"}`), "127.0.0.1:5050",
		http.Header{"Content-Type": {"application/json"}},
	)
	if denied.Code != http.StatusOK {
		t.Fatalf("deny status = %d, body=%s", denied.Code, denied.Body.String())
	}
	claimBody, _ := json.Marshal(map[string]string{
		"request_id": request.RequestID, "request_secret": request.RequestSecret,
	})
	claim := serve(t, daemon.handler, http.MethodPost, "/api/tailnet/access/claim", bytes.NewReader(claimBody), "127.0.0.1:4040", identity)
	if claim.Code != http.StatusForbidden || !strings.Contains(claim.Body.String(), `"denied"`) {
		t.Fatalf("denied claim status = %d, body=%s", claim.Code, claim.Body.String())
	}
}

func TestTailnetAcceptedClaimIsIdempotentAndExpiresUnacknowledgedCredential(t *testing.T) {
	daemon := newTestDaemon(t)
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	daemon.handler.tailnetAccess.now = func() time.Time { return now }
	daemon.handler.pair.setNow(func() time.Time { return now })
	identity := tailnetHeaders("uzair@example.com", "Uzair Haq")
	created := serve(
		t, daemon.handler, http.MethodPost, "/api/tailnet/access/request",
		strings.NewReader(`{"client_id":"33333333-3333-4333-8333-333333333333","name":"MacBook"}`),
		"127.0.0.1:4040", identity,
	)
	var request tailnetAccessRequestResponse
	decodeBody(t, created, &request)
	decision := serve(
		t, daemon.handler, http.MethodPost, "/api/tailnet/access/requests/"+request.RequestID,
		strings.NewReader(`{"decision":"accept"}`), "127.0.0.1:5050",
		http.Header{"Content-Type": {"application/json"}},
	)
	if decision.Code != http.StatusOK {
		t.Fatalf("accept status = %d, body=%s", decision.Code, decision.Body.String())
	}
	claimBody, _ := json.Marshal(map[string]string{
		"request_id": request.RequestID, "request_secret": request.RequestSecret,
	})
	first := serve(t, daemon.handler, http.MethodPost, "/api/tailnet/access/claim", bytes.NewReader(claimBody), "127.0.0.1:4040", identity)
	second := serve(t, daemon.handler, http.MethodPost, "/api/tailnet/access/claim", bytes.NewReader(claimBody), "127.0.0.1:4040", identity)
	var firstCredential, secondCredential pairingClaimResponse
	decodeBody(t, first, &firstCredential)
	decodeBody(t, second, &secondCredential)
	if firstCredential.Token == "" ||
		firstCredential.Token != secondCredential.Token ||
		firstCredential.DeviceID != secondCredential.DeviceID {
		t.Fatalf("claim was not idempotent: first=%#v second=%#v", firstCredential, secondCredential)
	}

	now = now.Add(tailnetAccessTTL + time.Second)
	expired := serve(
		t, daemon.handler, http.MethodGet, "/api/sessions", nil, "198.51.100.20:6060",
		http.Header{"Authorization": {"Bearer " + firstCredential.Token}},
	)
	if expired.Code != http.StatusUnauthorized {
		t.Fatalf("unacknowledged expired token status = %d, body=%s", expired.Code, expired.Body.String())
	}
	devices, err := daemon.handler.pair.devices.list()
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 0 {
		t.Fatalf("unacknowledged device became visible: %#v", devices)
	}
}

func TestTailnetPendingCredentialIsDurableAfterFirstAuthenticatedUse(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := newDeviceStore(t.TempDir()+"/devices.json", func() time.Time { return now })
	record, token, err := store.createPending("MacBook", now.Add(tailnetAccessTTL))
	if err != nil {
		t.Fatal(err)
	}
	if record.PendingUntil == nil {
		t.Fatal("tailnet credential was not created pending")
	}
	authorized, err := store.authorize(token)
	if err != nil || !authorized {
		t.Fatalf("first authorization = %v, err=%v", authorized, err)
	}
	now = now.Add(tailnetAccessTTL + time.Second)
	authorized, err = store.authorize(token)
	if err != nil || !authorized {
		t.Fatalf("acknowledged authorization after deadline = %v, err=%v", authorized, err)
	}
	devices, err := store.list()
	if err != nil || len(devices) != 1 {
		t.Fatalf("acknowledged devices = %#v, err=%v", devices, err)
	}
}

func TestExpiredPendingCredentialIsPurgedAfterStoreReload(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	path := t.TempDir() + "/devices.json"
	store := newDeviceStore(path, func() time.Time { return now })
	_, token, err := store.createPending("MacBook", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	reloaded := newDeviceStore(path, func() time.Time { return now })
	authorized, err := reloaded.authorize(token)
	if err != nil || authorized {
		t.Fatalf("reloaded expired authorization = %v, err=%v", authorized, err)
	}
	devices, err := reloaded.list()
	if err != nil || len(devices) != 0 {
		t.Fatalf("reloaded expired devices = %#v, err=%v", devices, err)
	}
	var stored deviceFile
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Devices) != 0 {
		t.Fatalf("expired pending device remained on disk: %#v", stored.Devices)
	}
}

func TestTailnetIssuanceGetsASeparateAcknowledgementWindow(t *testing.T) {
	daemon := newTestDaemon(t)
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	daemon.handler.tailnetAccess.now = func() time.Time { return now }
	daemon.handler.pair.setNow(func() time.Time { return now })
	identity := tailnetHeaders("uzair@example.com", "Uzair Haq")
	created := serve(
		t, daemon.handler, http.MethodPost, "/api/tailnet/access/request",
		strings.NewReader(`{"client_id":"44444444-4444-4444-8444-444444444444","name":"MacBook"}`),
		"127.0.0.1:4040", identity,
	)
	var request tailnetAccessRequestResponse
	decodeBody(t, created, &request)
	now = now.Add(tailnetAccessTTL - time.Second)
	decision := serve(
		t, daemon.handler, http.MethodPost, "/api/tailnet/access/requests/"+request.RequestID,
		strings.NewReader(`{"decision":"accept"}`), "127.0.0.1:5050",
		http.Header{"Content-Type": {"application/json"}},
	)
	if decision.Code != http.StatusOK {
		t.Fatalf("near-expiry accept status = %d, body=%s", decision.Code, decision.Body.String())
	}
	claimBody, _ := json.Marshal(map[string]string{
		"request_id": request.RequestID, "request_secret": request.RequestSecret,
	})
	claimed := serve(t, daemon.handler, http.MethodPost, "/api/tailnet/access/claim", bytes.NewReader(claimBody), "127.0.0.1:4040", identity)
	var credential pairingClaimResponse
	decodeBody(t, claimed, &credential)
	now = now.Add(30 * time.Second)
	authorized := serve(
		t, daemon.handler, http.MethodGet, "/api/sessions", nil, "198.51.100.20:6060",
		http.Header{"Authorization": {"Bearer " + credential.Token}},
	)
	if authorized.Code != http.StatusOK {
		t.Fatalf("near-expiry issued token status = %d, body=%s", authorized.Code, authorized.Body.String())
	}
}

// TestAccessRequestAliasesStayCompatibleAndNameTheCanonicalPath pins both
// spellings of the access-request collection. The frontend calls the canonical
// path and falls back to the legacy one; the sessions CLI calls the canonical
// one only. Neither may break, so the legacy path keeps working and says in
// its response headers that it is deprecated and where its successor is.
func TestAccessRequestAliasesStayCompatibleAndNameTheCanonicalPath(t *testing.T) {
	daemon := newTestDaemon(t)
	created := serve(
		t, daemon.handler, http.MethodPost, "/api/tailnet/access/request",
		strings.NewReader(`{"client_id":"77777777-7777-4777-8777-777777777777","name":"Alias Mac"}`),
		"127.0.0.1:4040", tailnetHeaders("alias@example.com", "Alias User"),
	)
	if created.Code != http.StatusAccepted {
		t.Fatalf("request status = %d, body=%s", created.Code, created.Body.String())
	}
	var pending tailnetAccessRequestResponse
	decodeBody(t, created, &pending)

	canonical := serve(t, daemon.handler, http.MethodGet, accessRequestsPath, nil, "127.0.0.1:5050", nil)
	legacy := serve(t, daemon.handler, http.MethodGet, legacyTailnetAccessRequestsPath, nil, "127.0.0.1:5050", nil)
	if canonical.Code != http.StatusOK || legacy.Code != http.StatusOK {
		t.Fatalf("canonical status=%d legacy status=%d", canonical.Code, legacy.Code)
	}
	if canonical.Body.String() != legacy.Body.String() {
		t.Fatalf("aliases disagree:\ncanonical=%s\nlegacy=%s", canonical.Body.String(), legacy.Body.String())
	}
	if canonical.Header().Get("Deprecation") != "" || canonical.Header().Get("Link") != "" {
		t.Fatalf("canonical path advertised deprecation: %#v", canonical.Header())
	}
	if legacy.Header().Get("Deprecation") != "true" {
		t.Fatalf("legacy path did not advertise deprecation: %#v", legacy.Header())
	}
	if got, want := legacy.Header().Get("Link"), `<`+accessRequestsPath+`>; rel="successor-version"`; got != want {
		t.Fatalf("legacy Link = %q, want %q", got, want)
	}

	decision := serve(
		t, daemon.handler, http.MethodPost, legacyTailnetAccessRequestsPath+"/"+pending.RequestID,
		strings.NewReader(`{"decision":"accept"}`), "127.0.0.1:5050",
		http.Header{"Content-Type": {"application/json"}},
	)
	if decision.Code != http.StatusOK {
		t.Fatalf("legacy decision status = %d, body=%s", decision.Code, decision.Body.String())
	}
	want := `<` + accessRequestsPath + "/" + pending.RequestID + `>; rel="successor-version"`
	if got := decision.Header().Get("Link"); got != want {
		t.Fatalf("legacy item Link = %q, want %q", got, want)
	}
}
