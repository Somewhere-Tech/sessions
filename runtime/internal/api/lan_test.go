package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/discovery"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type fakeBonjourRegistration struct {
	stopped bool
}

func (registration *fakeBonjourRegistration) Shutdown() error {
	registration.stopped = true
	return nil
}

func TestLANListenerLifecycleAndAuth(t *testing.T) {
	daemon := newTestDaemon(t)
	daemon.config.UserStateRoot = daemon.config.StateRoot
	daemon.config.SettingsPath = daemon.config.StateRoot + "/settings.json"
	notify := state.DefaultNotifySettings()
	notify.Waiting = false
	if err := state.SaveSettings(daemon.config.SettingsPath, state.Settings{Notify: &notify}); err != nil {
		t.Fatal(err)
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	daemon.config.Port = probe.Addr().(*net.TCPAddr).Port
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	daemon.handler = New(daemon.config, daemon.registry)
	t.Cleanup(func() { _ = daemon.handler.CloseLAN() })
	daemon.handler.lan.pickIP = func() (net.IP, error) { return net.ParseIP("127.0.0.1"), nil }
	var advertisedIP net.IP
	var advertisedPort int
	registration := &fakeBonjourRegistration{}
	daemon.handler.lan.advertise = discovery.AdvertiseFunc(func(ip net.IP, port int, _, _ string) (discovery.Registration, error) {
		advertisedIP = ip
		advertisedPort = port
		return registration, nil
	})

	enabled := serve(t, daemon.handler, http.MethodPost, "/api/lan", strings.NewReader(`{"enabled":true}`), "127.0.0.1:1", nil)
	if enabled.Code != http.StatusOK {
		t.Fatalf("enable status = %d, body=%s", enabled.Code, enabled.Body.String())
	}
	var current LANState
	decodeBody(t, enabled, &current)
	if !current.Enabled || current.URL == nil {
		t.Fatalf("enabled state = %#v", current)
	}
	if !current.Bonjour.Advertised || current.Bonjour.Service != discovery.ServiceType ||
		!advertisedIP.Equal(net.ParseIP("127.0.0.1")) || advertisedPort != daemon.config.Port {
		t.Fatalf("Bonjour state = %#v, ip=%v port=%d", current.Bonjour, advertisedIP, advertisedPort)
	}
	settings, err := state.LoadSettings(daemon.config.SettingsPath)
	if err != nil || !settings.LAN || settings.EffectiveNotify().Waiting {
		t.Fatalf("persisted settings after enable = %#v, %v", settings, err)
	}

	mainHealth := serve(t, daemon.handler, http.MethodGet, "/api/health", nil, "127.0.0.1:1", nil)
	lanHealth, err := http.Get(*current.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	lanHealthBody, err := io.ReadAll(lanHealth.Body)
	lanHealth.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if lanHealth.StatusCode != http.StatusOK {
		t.Fatalf("LAN health status = %d, body=%s", lanHealth.StatusCode, lanHealthBody)
	}
	var mainShape, lanShape map[string]any
	if err := json.Unmarshal(mainHealth.Body.Bytes(), &mainShape); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(lanHealthBody, &lanShape); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(mainShape, lanShape) {
		t.Fatalf("health shapes differ:\nmain=%#v\nlan=%#v", mainShape, lanShape)
	}

	protectedURL := *current.URL + "/api/sessions"
	request, _ := http.NewRequest(http.MethodGet, protectedURL, nil)
	request.Header.Set("X-Forwarded-For", "192.168.1.50")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("LAN protected route without token = %d, want 401", response.StatusCode)
	}
	request, _ = http.NewRequest(http.MethodGet, protectedURL, nil)
	request.Header.Set("X-Forwarded-For", "192.168.1.50")
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("LAN protected route with token = %d, want 200", response.StatusCode)
	}

	disabled := serve(t, daemon.handler, http.MethodPost, "/api/lan", strings.NewReader(`{"enabled":false}`), "127.0.0.1:1", nil)
	if disabled.Code != http.StatusOK {
		t.Fatalf("disable status = %d, body=%s", disabled.Code, disabled.Body.String())
	}
	if !registration.stopped {
		t.Fatal("Bonjour advertisement was not stopped with the LAN listener")
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, Timeout: time.Second}
	if response, err := client.Get(*current.URL + "/api/health"); err == nil {
		response.Body.Close()
		t.Fatalf("LAN listener still accepted a connection after disable: HTTP %d", response.StatusCode)
	}
	settings, err = state.LoadSettings(daemon.config.SettingsPath)
	if err != nil || settings.LAN || settings.EffectiveNotify().Waiting {
		t.Fatalf("persisted settings after disable = %#v, %v", settings, err)
	}
}

func TestLANRouteRequiresAuthorizationFromNonLoopbackPeer(t *testing.T) {
	daemon := newTestDaemon(t)
	response := serve(t, daemon.handler, http.MethodPost, "/api/lan", strings.NewReader(`{"enabled":true}`), "192.168.1.50:1234", nil)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized enable status = %d, body=%s", response.Code, response.Body.String())
	}
}

func TestLANActiveHostDoesNotWaitForBonjour(t *testing.T) {
	config := state.Config{Port: 0, SettingsPath: t.TempDir() + "/settings.json"}
	listener := newLANListener(config, http.NotFoundHandler(), machineIdentity{Name: "test", ID: "test-machine"})
	listener.pickIP = func() (net.IP, error) { return net.ParseIP("127.0.0.1"), nil }
	advertiseStarted := make(chan struct{})
	allowAdvertise := make(chan struct{})
	listener.advertise = func(net.IP, int, string, string) (discovery.Registration, error) {
		close(advertiseStarted)
		<-allowAdvertise
		return &fakeBonjourRegistration{}, nil
	}
	t.Cleanup(func() { _, _ = listener.disable(false) })

	enableDone := make(chan error, 1)
	go func() {
		_, err := listener.enable(false)
		enableDone <- err
	}()
	<-advertiseStarted

	hostDone := make(chan string, 1)
	go func() { hostDone <- listener.activeHost() }()
	select {
	case host := <-hostDone:
		if host != "127.0.0.1" {
			t.Fatalf("active host = %q, want 127.0.0.1", host)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("activeHost blocked behind Bonjour advertisement")
	}

	close(allowAdvertise)
	if err := <-enableDone; err != nil {
		t.Fatal(err)
	}
}

func TestRestoreLANWithoutInterfaceLogsAndContinues(t *testing.T) {
	daemon := newTestDaemon(t)
	daemon.config.UserStateRoot = daemon.config.StateRoot
	daemon.config.SettingsPath = daemon.config.StateRoot + "/settings.json"
	if err := state.SaveSettings(daemon.config.SettingsPath, state.Settings{LAN: true}); err != nil {
		t.Fatal(err)
	}
	daemon.handler = New(daemon.config, daemon.registry)
	daemon.handler.lan.pickIP = func() (net.IP, error) { return nil, errors.New("no test LAN interface") }
	var logs bytes.Buffer
	daemon.handler.RestoreLAN(func(format string, args ...any) { fmt.Fprintf(&logs, format, args...) })
	if current := daemon.handler.lan.state(); current.Enabled || current.URL != nil {
		t.Fatalf("restored state = %#v, want disabled", current)
	}
	if !strings.Contains(logs.String(), "continuing without LAN access") || !strings.Contains(logs.String(), "no test LAN interface") {
		t.Fatalf("restore log = %q", logs.String())
	}
	health := serve(t, daemon.handler, http.MethodGet, "/api/health", nil, "127.0.0.1:1", nil)
	if health.Code != http.StatusOK {
		t.Fatalf("daemon did not continue: status=%d body=%s", health.Code, health.Body.String())
	}
	if _, err := os.Stat(daemon.config.SettingsPath); err != nil {
		t.Fatal(err)
	}
}
