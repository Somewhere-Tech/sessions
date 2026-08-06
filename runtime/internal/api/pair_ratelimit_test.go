package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func claimFromPeer(t *testing.T, handler *Server, ticket, peer string) *http.Response {
	t.Helper()
	encoded, err := json.Marshal(map[string]string{"ticket": ticket, "name": "Phone"})
	if err != nil {
		t.Fatal(err)
	}
	return serve(t, handler, http.MethodPost, "/api/pair/claim", bytes.NewReader(encoded), peer, nil).Result()
}

// The finding: one global counter meant anyone who could reach /api/pair/claim
// could spend the whole budget on bad tickets and the user's own QR claim would
// then 429. The user's claim has to keep working while the attacker is blocked.
func TestPairClaimRateLimitIsPerSourceAddress(t *testing.T) {
	daemon := newTestDaemon(t)
	ticket := mintPairingTicket(t, daemon.handler, "Phone")

	const attacker = "198.51.100.99:5555"
	for attempt := 0; attempt < pairFailureLimit; attempt++ {
		if status := claimFromPeer(t, daemon.handler, "bogus.ticket", attacker).StatusCode; status != http.StatusGone {
			t.Fatalf("attacker attempt %d status = %d, want %d", attempt+1, status, http.StatusGone)
		}
	}
	blocked := claimFromPeer(t, daemon.handler, "bogus.ticket", attacker)
	if blocked.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("attacker was not rate limited: status = %d", blocked.StatusCode)
	}
	if blocked.Header.Get("Retry-After") == "" {
		t.Error("rate-limited response carries no Retry-After")
	}

	victim := claimFromPeer(t, daemon.handler, ticket.Ticket, "203.0.113.7:44444")
	if victim.StatusCode != http.StatusCreated {
		t.Fatalf("the user's own claim status = %d, want %d; an attacker still denies the pairing feature", victim.StatusCode, http.StatusCreated)
	}
}

func TestPairClaimRateLimitMessageStaysInstructional(t *testing.T) {
	daemon := newTestDaemon(t)
	const attacker = "198.51.100.99:5555"
	for attempt := 0; attempt <= pairFailureLimit; attempt++ {
		claimFromPeer(t, daemon.handler, "bogus.ticket", attacker)
	}
	response := serve(t, daemon.handler, http.MethodPost, "/api/pair/claim",
		strings.NewReader(`{"ticket":"bogus.ticket"}`), attacker, nil)
	body := response.Body.String()
	for _, want := range []string{"Wait one minute", "sessions pair"} {
		if !strings.Contains(body, want) {
			t.Errorf("rate-limit body = %q, want it to mention %q", body, want)
		}
	}
}

// The global counter still has to catch a spray from many addresses, otherwise
// the per-peer dimension just raises the attacker's cost by one IP per ten
// guesses.
func TestPairClaimGlobalCeilingBacksTheLimitUp(t *testing.T) {
	daemon := newTestDaemon(t)
	limited := false
	for attempt := 0; attempt < pairGlobalFailureLimit+pairFailureLimit; attempt++ {
		peer := fmt.Sprintf("198.51.100.%d:5555", attempt%250+1)
		if claimFromPeer(t, daemon.handler, "bogus.ticket", peer).StatusCode == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatalf("a spray of more than %d failures from fresh addresses was never limited", pairGlobalFailureLimit)
	}
}

func TestPairFailureTrackerBoundsItsMemory(t *testing.T) {
	daemon := newTestDaemon(t)
	service := daemon.handler.pair
	now := time.Now().UTC()
	service.setNow(func() time.Time { return now })

	service.mu.Lock()
	for index := 0; index < pairTrackedPeerLimit*3; index++ {
		service.recordFailureLocked(now, fmt.Sprintf("peer-%d", index))
	}
	tracked := len(service.peerFailures)
	service.mu.Unlock()

	if tracked > pairTrackedPeerLimit {
		t.Fatalf("tracked %d peers, want at most %d; a hostile peer can grow this map without bound", tracked, pairTrackedPeerLimit)
	}
}

func TestPairFailuresExpireWithTheWindow(t *testing.T) {
	daemon := newTestDaemon(t)
	service := daemon.handler.pair
	current := time.Now().UTC()
	service.setNow(func() time.Time { return current })

	const attacker = "198.51.100.99:5555"
	for attempt := 0; attempt <= pairFailureLimit; attempt++ {
		claimFromPeer(t, daemon.handler, "bogus.ticket", attacker)
	}
	if claimFromPeer(t, daemon.handler, "bogus.ticket", attacker).StatusCode != http.StatusTooManyRequests {
		t.Fatal("attacker was not rate limited before the window elapsed")
	}

	current = current.Add(pairFailureWindow + time.Second)
	if status := claimFromPeer(t, daemon.handler, "bogus.ticket", attacker).StatusCode; status != http.StatusGone {
		t.Fatalf("status after the window = %d, want %d; the limit never forgives", status, http.StatusGone)
	}
	service.mu.Lock()
	tracked := len(service.peerFailures)
	global := len(service.failedClaims)
	service.mu.Unlock()
	if tracked != 1 || global != 1 {
		t.Fatalf("after pruning: %d peers and %d global failures, want 1 and 1", tracked, global)
	}
}

func TestPairPeerKeyIdentifiesTheTransportPeer(t *testing.T) {
	for _, test := range []struct{ address, want string }{
		{"198.51.100.7:4242", "198.51.100.7"},
		{"198.51.100.7:9999", "198.51.100.7"},
		{"127.0.0.1:1", "127.0.0.1"},
		{"", "unknown"},
		// One host owns its whole /64, so rotating within it must not buy a
		// fresh budget.
		{"[2001:db8:abcd:1234::1]:443", "2001:db8:abcd:1234::/64"},
		{"[2001:db8:abcd:1234::beef]:8080", "2001:db8:abcd:1234::/64"},
	} {
		if got := pairPeerKey(test.address); got != test.want {
			t.Errorf("pairPeerKey(%q) = %q, want %q", test.address, got, test.want)
		}
	}
	if pairPeerKey("[2001:db8:abcd:1234::1]:443") == pairPeerKey("[2001:db8:9999:1234::1]:443") {
		t.Error("two different /64 prefixes collapsed onto one budget")
	}
}
