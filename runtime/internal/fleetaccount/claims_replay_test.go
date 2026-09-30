package fleetaccount

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func futureClaimFixture(t *testing.T) (*Manager, AccountClaim, *time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	future := now.Add(4 * time.Minute)
	var directory []Machine
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		writeTestJSON(response, map[string]any{"machines": directory})
	}))
	t.Cleanup(server.Close)
	host := testClaimManager(t, server.URL, "host", &now)
	requester := testClaimManager(t, server.URL, "requester", &future)
	public, err := requester.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	directory = []Machine{{ID: "requester", MachinePublicKey: public}}
	claim, err := requester.CreateAccountClaim("host")
	if err != nil {
		t.Fatal(err)
	}
	return host, claim, &now
}

type expiredClaimTransport struct {
	clock *atomic.Int64
}

func (transport expiredClaimTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := http.DefaultTransport.RoundTrip(request)
	transport.clock.Add(int64(10 * time.Minute / time.Second))
	return response, err
}

func TestAccountClaimDirectoryLookupCannotAdmitExpiredClaim(t *testing.T) {
	host, claim, now := futureClaimFixture(t)
	var clock atomic.Int64
	clock.Store(now.Unix())
	host.now = func() time.Time { return time.Unix(clock.Load(), 0) }
	host.cloud.client = &http.Client{Transport: expiredClaimTransport{clock: &clock}}
	if _, err := host.VerifyAccountClaim(context.Background(), claim); !errors.Is(err, ErrClaimExpired) {
		t.Fatalf("claim expired during directory lookup = %v, want expired", err)
	}
}

func TestAccountClaimReplayRejectedUntilSignedTimestampExpires(t *testing.T) {
	host, claim, now := futureClaimFixture(t)
	if _, err := host.VerifyAccountClaim(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(5*time.Minute + time.Second)
	if _, err := host.VerifyAccountClaim(context.Background(), claim); !errors.Is(err, ErrClaimReplay) {
		t.Fatalf("future-dated claim replay after five minutes = %v, want replay refusal", err)
	}
	*now = now.Add(239 * time.Second)
	if _, err := host.VerifyAccountClaim(context.Background(), claim); !errors.Is(err, ErrClaimReplay) {
		t.Fatalf("claim replay at inclusive timestamp boundary = %v, want replay refusal", err)
	}
	*now = now.Add(time.Second)
	if _, err := host.VerifyAccountClaim(context.Background(), claim); !errors.Is(err, ErrClaimExpired) {
		t.Fatalf("claim after signed timestamp expiry = %v, want expired", err)
	}
}

func TestAccountClaimReplayRejectedAfterManagerRestartAndLogout(t *testing.T) {
	host, claim, now := futureClaimFixture(t)
	if _, err := host.VerifyAccountClaim(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if err := host.state.clear(); err != nil {
		t.Fatal(err)
	}
	if err := host.state.update(func(state *persistedState) {
		state.Tokens = TokenPair{AccessToken: "fixture-access", RefreshToken: "fixture-refresh"}
	}); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(Options{
		BaseURL: host.cloud.baseURL, AccountPath: host.state.path, KeyPath: host.keys.path,
		MachineID: host.machineID, Now: func() time.Time { return *now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.VerifyAccountClaim(context.Background(), claim); !errors.Is(err, ErrClaimReplay) {
		t.Fatalf("claim replay after restart/logout = %v, want replay refusal", err)
	}
}
