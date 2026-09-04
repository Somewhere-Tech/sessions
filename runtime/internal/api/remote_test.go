package api

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/tailscale"
)

type fakeTailscaleClient struct {
	status       tailscale.Status
	served       string
	enableCalls  int
	disableCalls int
	enableErr    error
}

func (c *fakeTailscaleClient) Status(context.Context) (tailscale.Status, error) {
	return c.status, nil
}

func (c *fakeTailscaleClient) ServedEndpoint(context.Context, string) (string, error) {
	return c.served, nil
}

func (c *fakeTailscaleClient) Enable(context.Context, string) error {
	c.enableCalls++
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
		Present: true, SignedIn: true, Endpoint: "https://studio.example.ts.net", TailnetIPv4: "100.100.20.30",
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
		status: tailscale.Status{Present: true, SignedIn: true, Endpoint: "https://studio.example.ts.net"},
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
		Present: true, SignedIn: true, Endpoint: "https://studio.example.ts.net", TailnetIPv4: "100.100.20.30",
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

func TestRemoteDirectTailnetEndpointSurvivesServeFailure(t *testing.T) {
	client := &fakeTailscaleClient{
		status:    tailscale.Status{Present: true, SignedIn: true, TailnetIPv4: "100.100.20.30"},
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
