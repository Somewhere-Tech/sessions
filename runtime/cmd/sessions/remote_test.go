package main

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestDaemonListenDoesNotTrustHealthRoutingMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"name":"sessionsd","listen":{"host":"attacker.example","port":443}}`))
	}))
	defer server.Close()
	client, err := newAPIClient(server.URL, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	application := &app{api: client, port: "8787"}
	var status tailscaleStatus
	if err := json.Unmarshal([]byte(`{"Self":{"TailscaleIPs":[]}}`), &status); err != nil {
		t.Fatal(err)
	}
	host, port, err := application.daemonListen(status)
	if err != nil {
		t.Fatal(err)
	}
	if "http://"+net.JoinHostPort(host, strconv.Itoa(port)) != server.URL {
		t.Fatalf("health selected a different backend: %s:%d", host, port)
	}
}

func TestServeStatusHelpers(t *testing.T) {
	status := &serveJSON{Web: map[string]serveWeb{
		"sessions.example.ts.net:443": {Handlers: map[string]serveHandler{
			"/": {Proxy: "http://127.0.0.1:8787"},
		}},
	}}
	if got := endpointFromServeJSON(status, "http://127.0.0.1:8787/"); got != "https://sessions.example.ts.net:443" {
		t.Fatalf("endpoint=%q", got)
	}
	endpoint, proxy := rootHandlerFromServeJSON(status)
	if endpoint != "https://sessions.example.ts.net:443" || proxy != "http://127.0.0.1:8787" {
		t.Fatalf("root endpoint=%q proxy=%q", endpoint, proxy)
	}
	if got := endpointFromServeText("Available on your tailnet:\nhttps://sessions.example.ts.net"); got != "https://sessions.example.ts.net" {
		t.Fatalf("text endpoint=%q", got)
	}
}

func TestRemoteDiagnosticsHelpers(t *testing.T) {
	target, err := formatDaemonTarget("::1", 8787)
	if err != nil || target != "http://[::1]:8787" {
		t.Fatalf("target=%q err=%v", target, err)
	}
	if _, err := formatDaemonTarget("127.0.0.1", 70000); err == nil {
		t.Fatal("invalid port accepted")
	}
	if !isMagicDNSResolutionError(&net.DNSError{Err: "no such host", Name: "sessions.example.ts.net", IsNotFound: true}) {
		t.Fatal("DNS not-found was not classified")
	}
	if isMagicDNSResolutionError(&net.DNSError{Err: "temporary failure", Name: "sessions.example.ts.net", IsTemporary: true}) {
		t.Fatal("temporary DNS failure must not bypass remote verification")
	}
	if isMagicDNSResolutionError(errors.New("TLS certificate expired")) {
		t.Fatal("TLS error misclassified as MagicDNS")
	}
}
