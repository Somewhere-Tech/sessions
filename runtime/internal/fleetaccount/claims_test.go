package fleetaccount

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestAccountClaimVerificationRejectsBadReplayOtherAccountAndExpired(t *testing.T) {
	now := time.Date(2026, 9, 3, 22, 0, 0, 0, time.UTC)
	var directory []Machine
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/machines/index" {
			http.NotFound(response, request)
			return
		}
		writeTestJSON(response, map[string]any{"machines": directory})
	}))
	defer server.Close()

	host := testClaimManager(t, server.URL, "host-machine", &now)
	requester := testClaimManager(t, server.URL, "requester-machine", &now)
	otherAccount := testClaimManager(t, server.URL, "requester-machine", &now)
	requesterPublic, err := requester.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	directory = []Machine{{ID: "requester-machine", Name: "Uzair's phone", MachinePublicKey: requesterPublic}}

	valid, err := requester.CreateAccountClaim("host-machine")
	if err != nil {
		t.Fatal(err)
	}
	bad := valid
	bad.Signature = "not-a-signature"
	if _, err := host.VerifyAccountClaim(context.Background(), bad); !errors.Is(err, ErrClaimInvalid) {
		t.Fatalf("bad signature error = %v", err)
	}
	if device, err := host.VerifyAccountClaim(context.Background(), valid); err != nil || device.Name != "Uzair's phone" {
		t.Fatalf("valid claim device=%+v err=%v", device, err)
	}
	if _, err := host.VerifyAccountClaim(context.Background(), valid); !errors.Is(err, ErrClaimReplay) {
		t.Fatalf("replay error = %v", err)
	}

	foreign, err := otherAccount.CreateAccountClaim("host-machine")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.VerifyAccountClaim(context.Background(), foreign); !errors.Is(err, ErrClaimInvalid) {
		t.Fatalf("other account key error = %v", err)
	}

	expired, err := requester.CreateAccountClaim("host-machine")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(6 * time.Minute)
	if _, err := host.VerifyAccountClaim(context.Background(), expired); !errors.Is(err, ErrClaimExpired) {
		t.Fatalf("expired claim error = %v", err)
	}
}

func testClaimManager(t *testing.T, baseURL, machineID string, now *time.Time) *Manager {
	t.Helper()
	root := t.TempDir()
	manager, err := New(Options{
		BaseURL: baseURL, AccountPath: filepath.Join(root, "account.json"),
		KeyPath: filepath.Join(root, "key.json"), MachineID: machineID,
		MachineName: machineID, Now: func() time.Time { return *now },
	})
	if err != nil {
		t.Fatal(err)
	}
	err = manager.state.update(func(state *persistedState) {
		state.Tokens = TokenPair{AccessToken: "access", RefreshToken: "refresh"}
		state.User = User{ID: "owner-one", Email: "owner@example.com"}
	})
	if err != nil {
		t.Fatal(err)
	}
	// Force creation here so the test's directory key is stable before a claim.
	if _, _, err := manager.keys.loadOrCreate(); err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestMachinesRejectsMalformedDirectoryResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"machines":{}}`))
	}))
	defer server.Close()
	now := time.Now()
	manager := testClaimManager(t, server.URL, "machine", &now)
	_, err := manager.Machines(context.Background())
	if err == nil {
		t.Fatal("malformed directory unexpectedly succeeded")
	}
	var syntax *json.UnmarshalTypeError
	if errors.As(err, &syntax) {
		t.Fatalf("directory leaked decoder detail: %v", err)
	}
}
