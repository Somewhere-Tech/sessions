package main

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

// The Mini spent 150 s at 100-170% CPU after a restart and nothing could say
// why: /api/health/deep reported pprof off, and `doctor --cpu-profile` refuses
// without it. Asking somebody to restart the daemon with an environment
// variable after the burst has passed is not an answer, so it is on — bound to
// loopback, on a port the OS picks, exposing the Go runtime's own stack traces
// and nothing about anybody's work.
func TestPprofIsOnByDefaultAndLoopbackOnly(t *testing.T) {
	listener, err := startPprof("")
	if err != nil || listener == nil {
		t.Fatalf("default pprof = %#v, %v", listener, err)
	}
	t.Cleanup(listener.close)
	if !profileAddressIsLoopbackForTest(listener.address()) {
		t.Fatalf("default pprof listened on %q, which is not loopback", listener.address())
	}
	// And it can still be turned off, for a machine whose owner wants it off.
	for _, off := range []string{"off", "OFF", "false", "0"} {
		if disabled, err := startPprof(off); err != nil || disabled != nil {
			t.Fatalf("startPprof(%q) = %#v, %v; want it off", off, disabled, err)
		}
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

func profileAddressIsLoopbackForTest(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
