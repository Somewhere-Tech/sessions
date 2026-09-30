package api

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/tailscale"
)

type fakeTailscaleClient struct {
	status        tailscale.Status
	served        string
	enableCalls   int
	disableCalls  int
	enableErr     error
	serveNames    []string
	enableTargets []string
}

func (c *fakeTailscaleClient) Status(context.Context) (tailscale.Status, error) {
	return c.status, nil
}

func (c *fakeTailscaleClient) ServedEndpoint(_ context.Context, _, dnsName string) (string, error) {
	c.serveNames = append(c.serveNames, dnsName)
	return c.served, nil
}

func (c *fakeTailscaleClient) Enable(_ context.Context, target string) error {
	c.enableCalls++
	c.enableTargets = append(c.enableTargets, target)
	if c.enableErr != nil {
		return c.enableErr
	}
	c.served = c.status.Endpoint
	return nil
}

func (c *fakeTailscaleClient) Disable(context.Context) error {
	c.disableCalls++
	c.served = ""
	return nil
}

func TestRemotePreviewReportsEndpointsWithoutChangingServe(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	client := &fakeTailscaleClient{status: tailscale.Status{
		Present: true, SignedIn: true, DNSName: "studio.example.ts.net",
		Endpoint: "https://studio.example.ts.net", TailnetIPv4: "100.100.20.30",
	}}
	manager := newRemoteManager(state.Config{Host: "127.0.0.1", Port: 8897}, settingsPath)
	manager.preview = true
	manager.newClient = func() (tailscaleClient, error) { return client, nil }
	manager.check(context.Background())
	got := manager.state()
	if !got.Auto || !got.Present || !got.SignedIn || !got.Preview || got.Enabled ||
		got.Endpoint != "https://studio.example.ts.net" || got.TailnetIPEndpoint != "http://100.100.20.30:8897" {
		t.Fatalf("preview state = %#v", got)
	}
	if client.enableCalls != 0 || client.disableCalls != 0 {
		t.Fatalf("preview mutated Tailscale: enable=%d disable=%d", client.enableCalls, client.disableCalls)
	}
}

func TestRemoteAutoOffPersistsAndDisablesOwnedServe(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	client := &fakeTailscaleClient{
		status: tailscale.Status{
			Present: true, SignedIn: true, DNSName: "studio.example.ts.net", Endpoint: "https://studio.example.ts.net",
		},
		served: "https://studio.example.ts.net",
	}
	manager := newRemoteManager(state.Config{Host: "127.0.0.1", Port: 8787}, settingsPath)
	manager.newClient = func() (tailscaleClient, error) { return client, nil }
	got, err := manager.setAuto(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := state.LoadSettings(settingsPath)
	if err != nil || settings.EffectiveRemote().Auto || got.Auto || got.Enabled {
		t.Fatalf("setting=%#v state=%#v err=%v", settings.Remote, got, err)
	}
	if client.disableCalls != 1 || client.enableCalls != 0 {
		t.Fatalf("serve calls: enable=%d disable=%d", client.enableCalls, client.disableCalls)
	}
}

func TestRemoteDefaultEnablesServeAndDirectTailnetEndpoint(t *testing.T) {
	client := &fakeTailscaleClient{status: tailscale.Status{
		Present: true, SignedIn: true, DNSName: "studio.example.ts.net",
		Endpoint: "https://studio.example.ts.net", TailnetIPv4: "100.100.20.30",
	}}
	manager := newRemoteManager(state.Config{Host: "127.0.0.1", Port: 8787}, filepath.Join(t.TempDir(), "settings.json"))
	manager.newClient = func() (tailscaleClient, error) { return client, nil }
	manager.check(context.Background())
	got := manager.state()
	if !got.Auto || !got.Enabled || got.Endpoint != client.status.Endpoint ||
		got.TailnetIPEndpoint != "http://100.100.20.30:8787" || client.enableCalls != 1 {
		t.Fatalf("automatic state = %#v, enable calls = %d", got, client.enableCalls)
	}
}

func TestRemoteCurrentNameKeepsExistingServe(t *testing.T) {
	client := &fakeTailscaleClient{
		status: tailscale.Status{
			Present: true, SignedIn: true, DNSName: "studio.example.ts.net", Endpoint: "https://studio.example.ts.net",
		},
		served: "https://studio.example.ts.net",
	}
	manager := newRemoteManager(state.Config{Host: "127.0.0.1", Port: 8787}, filepath.Join(t.TempDir(), "settings.json"))
	manager.newClient = func() (tailscaleClient, error) { return client, nil }
	manager.check(context.Background())

	got := manager.state()
	if got.Endpoint != client.served || got.CurrentDNSName != client.status.DNSName ||
		got.ServedDNSName != client.status.DNSName || client.enableCalls != 0 {
		t.Fatalf("current-name state = %#v, enable calls = %d", got, client.enableCalls)
	}
}

func TestRemoteDirectTailnetEndpointSurvivesServeFailure(t *testing.T) {
	client := &fakeTailscaleClient{
		status: tailscale.Status{
			Present: true, SignedIn: true, DNSName: "studio.example.ts.net",
			Endpoint: "https://studio.example.ts.net", TailnetIPv4: "100.100.20.30",
		},
		enableErr: errors.New("Serve needs approval"),
	}
	manager := newRemoteManager(state.Config{Host: "127.0.0.1", Port: 8787}, filepath.Join(t.TempDir(), "settings.json"))
	manager.newClient = func() (tailscaleClient, error) { return client, nil }
	manager.check(context.Background())
	got := manager.state()
	if !got.Enabled || got.Endpoint != "" || got.TailnetIPEndpoint != "http://100.100.20.30:8787" {
		t.Fatalf("direct fallback state = %#v", got)
	}
}

func TestRemoteNameChangeReconfiguresServeBeforeAdvertising(t *testing.T) {
	client := &fakeTailscaleClient{
		status: tailscale.Status{
			Present: true, SignedIn: true, DNSName: "mac-mini-313.tail61417e.ts.net",
			Endpoint: "https://mac-mini-313.tail61417e.ts.net",
		},
		served: "https://mac-mini-8.tail61417e.ts.net",
	}
	manager := newRemoteManager(state.Config{Host: "127.0.0.1", Port: 8787}, filepath.Join(t.TempDir(), "settings.json"))
	manager.newClient = func() (tailscaleClient, error) { return client, nil }
	logs := []string{}
	manager.logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	manager.check(context.Background())

	got := manager.state()
	if got.Endpoint != client.status.Endpoint || got.CurrentDNSName != client.status.DNSName ||
		got.ServedDNSName != client.status.DNSName || client.enableCalls != 1 {
		t.Fatalf("renamed state = %#v, enable calls = %d", got, client.enableCalls)
	}
	if want := []string{"tailnet name changed: re-serving as mac-mini-313.tail61417e.ts.net"}; !reflect.DeepEqual(logs, want) {
		t.Fatalf("logs = %#v, want %#v", logs, want)
	}
	if want := []string{client.status.DNSName, client.status.DNSName}; !reflect.DeepEqual(client.serveNames, want) {
		t.Fatalf("Serve status DNS names = %#v, want %#v", client.serveNames, want)
	}
	if want := []string{"http://127.0.0.1:8787"}; !reflect.DeepEqual(client.enableTargets, want) {
		t.Fatalf("Serve targets = %#v, want %#v", client.enableTargets, want)
	}
}

func TestRemoteNameMismatchIsNeverAdvertisedWhenReconfigureFails(t *testing.T) {
	client := &fakeTailscaleClient{
		status: tailscale.Status{
			Present: true, SignedIn: true, DNSName: "mac-mini-313.tail61417e.ts.net",
			Endpoint: "https://mac-mini-313.tail61417e.ts.net",
		},
		served: "https://mac-mini-8.tail61417e.ts.net", enableErr: errors.New("Serve rejected update"),
	}
	manager := newRemoteManager(state.Config{Host: "127.0.0.1", Port: 8787}, filepath.Join(t.TempDir(), "settings.json"))
	manager.newClient = func() (tailscaleClient, error) { return client, nil }
	advertised := "not called"
	manager.onEndpoints = func(endpoint, _ string) error { advertised = endpoint; return nil }
	manager.check(context.Background())

	got := manager.state()
	if got.Endpoint != "" || advertised != "" || got.ServedDNSName != "mac-mini-8.tail61417e.ts.net" ||
		got.CurrentDNSName != client.status.DNSName || !strings.Contains(got.LastError, "reconfigure Tailscale Serve") {
		t.Fatalf("failed rename state = %#v, advertised = %q", got, advertised)
	}
}

func TestRemoteNameMismatchIsConfinedToDeepHealth(t *testing.T) {
	manager := newRemoteManager(state.Config{}, filepath.Join(t.TempDir(), "settings.json"))
	manager.current = RemoteState{
		CurrentDNSName: "mac-mini-313.tail61417e.ts.net",
		ServedDNSName:  "mac-mini-8.tail61417e.ts.net",
	}
	server := &Server{remote: manager}
	if _, exposed := server.tailscaleHealth()["servedDNSName"]; exposed {
		t.Fatal("unauthenticated health exposed the stale Serve name")
	}
	deep := server.tailscaleDeepHealth()
	if deep["currentDNSName"] != manager.current.CurrentDNSName || deep["servedDNSName"] != manager.current.ServedDNSName {
		t.Fatalf("deep Tailscale health = %#v", deep)
	}
}
