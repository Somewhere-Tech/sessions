package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPprofIsOffByDefaultAndLoopbackOnly(t *testing.T) {
	listener, err := startPprof("")
	if err != nil || listener != nil {
		t.Fatalf("default pprof = %#v, %v", listener, err)
	}
	for _, address := range []string{"0.0.0.0:6060", "192.0.2.1:6060", "localhost:6060", ":6060"} {
		if listener, err := startPprof(address); err == nil || listener != nil {
			t.Fatalf("startPprof(%q) = %#v, %v; want refusal", address, listener, err)
		}
	}
}

func TestPprofServesStandardHandlersOnLoopback(t *testing.T) {
	listener, err := startPprof("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(listener.close)
	response, err := http.Get("http://" + listener.address() + "/debug/pprof/goroutine?debug=1")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "goroutine profile") {
		t.Fatalf("pprof response = %s %q", response.Status, body)
	}
}
