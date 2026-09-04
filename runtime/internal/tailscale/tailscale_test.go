package tailscale

import (
	"os"
	"strings"
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
	if status.DNSName != "studio-mac.example.ts.net" {
		t.Fatalf("DNS name = %q", status.DNSName)
	}
	if len(status.TailscaleIPs) != 2 || status.TailscaleIPs[0] != "100.64.12.34" || status.TailnetIPv4 != "100.64.12.34" {
		t.Fatalf("Tailscale IPs = %#v", status.TailscaleIPs)
	}
}

func TestServeStatusNameFixtures(t *testing.T) {
	statusFixture, err := os.ReadFile("testdata/status-renamed.json")
	if err != nil {
		t.Fatal(err)
	}
	status, err := ParseStatus(statusFixture)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		fixture  string
		wantHost string
	}{
		{name: "normal", fixture: "serve-current-name.json", wantHost: status.DNSName},
		{name: "mismatch", fixture: "serve-stale-name.json", wantHost: "mac-mini-8.tail61417e.ts.net"},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded, readErr := os.ReadFile("testdata/" + test.fixture)
			if readErr != nil {
				t.Fatal(readErr)
			}
			endpoint, parseErr := ParseServedEndpoint(encoded, "http://127.0.0.1:8787", status.DNSName)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			if host := EndpointHost(endpoint); host != test.wantHost {
				t.Fatalf("served endpoint = %q (host %q), want host %q", endpoint, host, test.wantHost)
			}
		})
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

func TestParseStatusReportsProseFromTheCLI(t *testing.T) {
	_, err := ParseStatus([]byte("The Tailscale GUI failed to start: The operation couldn’t be completed. (Tailscale.CLIError error 3.)\n"))
	if err == nil || !strings.Contains(err.Error(), "Tailscale CLI: The Tailscale GUI failed to start") {
		t.Fatalf("ParseStatus() error = %v, want the CLI sentence", err)
	}
}

func TestCLIEnvironmentAddsSHLVLOnce(t *testing.T) {
	env := cliEnvironment([]string{"PATH=/usr/bin"})
	if len(env) != 2 || env[1] != "SHLVL=1" {
		t.Fatalf("cliEnvironment() = %v, want SHLVL=1 appended", env)
	}
	again := cliEnvironment(env)
	if len(again) != 2 {
		t.Fatalf("cliEnvironment() added SHLVL twice: %v", again)
	}
}
