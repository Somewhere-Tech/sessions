//go:build darwin

package discovery

import (
	"net"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"
)

func TestDNSSDProxyArgumentsPinSelectedPrivateAddress(t *testing.T) {
	got := dnsSDProxyArguments(net.ParseIP("192.168.1.20"), 8787, "Studio Mac", "sessions-abc123")
	want := []string{
		"-P",
		"Studio Mac",
		"_sessions._tcp",
		"local.",
		"8787",
		"sessions-abc123.local.",
		"192.168.1.20",
		"sessions=1",
		"api=1",
		"approval=required",
		"transport=http",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dnsSDProxyArguments() = %#v, want %#v", got, want)
	}
}

// TestDarwinAdvertiseSystemSmoke is opt-in so the ordinary unit suite does not
// publish a service. Release acceptance uses it from one Mac while a second
// machine browses and verifies the advertised Sessions health endpoint.
func TestDarwinAdvertiseSystemSmoke(t *testing.T) {
	addressText := os.Getenv("SESSIONS_BONJOUR_SMOKE_ADDRESS")
	if addressText == "" {
		t.Skip("set SESSIONS_BONJOUR_SMOKE_ADDRESS for a two-machine acceptance test")
	}
	address := net.ParseIP(addressText)
	if address == nil {
		t.Fatalf("invalid SESSIONS_BONJOUR_SMOKE_ADDRESS %q", addressText)
	}
	port := 8787
	if portText := os.Getenv("SESSIONS_BONJOUR_SMOKE_PORT"); portText != "" {
		parsed, err := strconv.Atoi(portText)
		if err != nil {
			t.Fatalf("invalid SESSIONS_BONJOUR_SMOKE_PORT %q: %v", portText, err)
		}
		port = parsed
	}
	registration, err := Advertise(address, port, "Sessions Bonjour smoke", "smoke-test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := registration.Shutdown(); err != nil {
			t.Errorf("shutdown Bonjour smoke registration: %v", err)
		}
	}()
	t.Logf("advertising Sessions on %s:%d for cross-machine verification", address, port)
	time.Sleep(15 * time.Second)
}
