package fleetaccount

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestVerifyLoginRegistersMachinePayloadAndKeepsTokensOnNetworkFailure(t *testing.T) {
	now := time.Date(2026, 9, 3, 20, 15, 0, 0, time.UTC)
	wantEndpoints := Endpoints{
		LAN: "http://192.168.1.20:8787", Tailnet: "https://mini.example.ts.net",
	}
	var registered Registration
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/auth-token/magic-link/verify":
			writeTestJSON(response, map[string]any{
				"user":  map[string]string{"id": "user-one", "email": "owner@example.com"},
				"token": "access-one", "refresh_token": "refresh-one", "session_token": "session-one",
			})
		case "/api/machines/register":
			if request.Header.Get("Authorization") != "Bearer access-one" || request.Header.Get("X-Refresh-Token") != "refresh-one" {
				t.Errorf("registration auth headers = %#v", request.Header)
			}
			body, _ := io.ReadAll(request.Body)
			if err := json.Unmarshal(body, &registered); err != nil {
				t.Error(err)
			}
			verifyTestSignature(t, request, body, registered.MachinePublicKey)
			writeTestJSON(response, map[string]bool{"ok": true})
		default:
			http.NotFound(response, request)
		}
	}))
	root := t.TempDir()
	manager, err := New(Options{
		BaseURL: server.URL, AccountPath: filepath.Join(root, "account.json"), KeyPath: filepath.Join(root, "key.json"),
		MachineID: "machine-one", MachineName: "Studio Mac", DaemonVersion: "0.2.27",
		Endpoints: func() Endpoints { return wantEndpoints }, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	status, err := manager.VerifyMagicLink(context.Background(), "123456")
	if err != nil {
		t.Fatal(err)
	}
	if !status.SignedIn || status.User == nil || status.User.Email != "owner@example.com" || status.LastRegistrationAt != now.Format(time.RFC3339) {
		t.Fatalf("status after login = %+v", status)
	}
	want := Registration{
		MachineID: "machine-one", Name: "Studio Mac", MachinePublicKey: registered.MachinePublicKey,
		EndpointsJSON: wantEndpoints, DaemonVersion: "0.2.27",
	}
	if !reflect.DeepEqual(registered, want) {
		t.Fatalf("registration = %+v, want %+v", registered, want)
	}
	server.Close()
	if err := manager.Heartbeat(context.Background()); err == nil {
		t.Fatal("heartbeat unexpectedly succeeded after network closed")
	}
	status, err = manager.Status()
	if err != nil || !status.SignedIn {
		t.Fatalf("network failure logged account out: status=%+v err=%v", status, err)
	}
}

func TestNormalizeMagicTokenAcceptsCodeOrLink(t *testing.T) {
	tests := map[string]string{
		" 123456 ": "123456",
		"https://sessions-fleet.example/verify?token=abc": "abc",
		"https://sessions-fleet.example/verify":           "https://sessions-fleet.example/verify",
	}
	for input, want := range tests {
		if got := normalizeMagicToken(input); got != want {
			t.Errorf("normalizeMagicToken(%q) = %q, want %q", input, got, want)
		}
	}
}

func verifyTestSignature(t *testing.T, request *http.Request, body []byte, publicText string) {
	t.Helper()
	public, err := base64.RawURLEncoding.DecodeString(publicText)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(request.Header.Get(signatureHeader))
	if err != nil {
		t.Fatal(err)
	}
	canonical := canonicalRequest(
		request.Header.Get(machineIDHeader), request.Header.Get(timestampHeader), request.Header.Get(nonceHeader),
		request.Method, request.URL.Path, body,
	)
	if !ed25519.Verify(ed25519.PublicKey(public), canonical, signature) {
		t.Fatal("registration signature is invalid")
	}
}

func writeTestJSON(response http.ResponseWriter, value any) {
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(value)
}
