package tailscale

import (
	"os"
	"testing"
)

func TestParseStatusFixture(t *testing.T) {
	encoded, err := os.ReadFile("testdata/status-running.json")
	if err != nil {
		t.Fatal(err)
	}
	status, err := ParseStatus(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Present || !status.SignedIn || status.Endpoint != "https://studio-mac.example.ts.net" {
		t.Fatalf("status = %#v", status)
	}
	if len(status.TailscaleIPs) != 2 || status.TailscaleIPs[0] != "100.64.12.34" || status.TailnetIPv4 != "100.64.12.34" {
		t.Fatalf("Tailscale IPs = %#v", status.TailscaleIPs)
	}
}

func TestTailnetIPv4RejectsNonTailscaleAddresses(t *testing.T) {
	if got := TailnetIPv4([]string{"192.168.1.2", "203.0.113.8", "100.63.255.255", "100.128.0.1"}); got != "" {
		t.Fatalf("selected non-tailnet address %q", got)
	}
	if got := TailnetIPv4([]string{"192.168.1.2", "100.100.20.30"}); got != "100.100.20.30" {
		t.Fatalf("selected address %q", got)
	}
}

func TestParseStatusSignedOutAndMalformed(t *testing.T) {
	status, err := ParseStatus([]byte(`{"BackendState":"NeedsLogin"}`))
	if err != nil || !status.Present || status.SignedIn || status.Endpoint != "" {
		t.Fatalf("signed-out status = %#v, %v", status, err)
	}
	if _, err := ParseStatus([]byte(`{"BackendState":`)); err == nil {
		t.Fatal("malformed status was accepted")
	}
}
