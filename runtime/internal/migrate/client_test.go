package migrate

import "testing"

func TestRelayClientKeepsRelayPathSeparateFromDisplayedDestination(t *testing.T) {
	client, err := NewRelayClient("http://127.0.0.1:8897", "http://10.0.0.2:8898", "machine-mini")
	if err != nil {
		t.Fatal(err)
	}
	if got := client.requestURL("/api/migrate/receive"); got != "http://127.0.0.1:8897/api/fleet/machine-mini/api/migrate/receive" {
		t.Fatalf("request URL = %q", got)
	}
	if client.Endpoint() != "http://10.0.0.2:8898" {
		t.Fatalf("display endpoint = %q", client.Endpoint())
	}
}
