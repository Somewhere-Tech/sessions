package migrate

import (
	"strings"
	"testing"
)

func TestMigrationEndpointRejectsCredentialsWithoutEchoingThem(t *testing.T) {
	for _, endpoint := range []string{"https://alice:secret@example.com", "https://alice:secret%@example.com"} {
		for _, relay := range []bool{false, true} {
			var err error
			if relay {
				_, err = NewRelayClient("http://127.0.0.1:8897", endpoint, "mini")
			} else {
				_, err = NewClient(endpoint, "")
			}
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsafe endpoint error: %v", err)
			}
		}
	}
}

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
