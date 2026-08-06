package api

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

const testLANURL = "http://192.168.7.42:8787"

// fakeEnabledLAN puts the listener into its enabled state without binding a
// socket. Only the reported state matters to /api/health.
func fakeEnabledLAN(t *testing.T, daemon testDaemon) {
	t.Helper()
	daemon.handler.lan.mu.Lock()
	daemon.handler.lan.server = &http.Server{}
	daemon.handler.lan.host = "192.168.7.42"
	daemon.handler.lan.url = testLANURL
	daemon.handler.lan.mu.Unlock()
	t.Cleanup(func() {
		daemon.handler.lan.mu.Lock()
		daemon.handler.lan.server = nil
		daemon.handler.lan.url = ""
		daemon.handler.lan.host = ""
		daemon.handler.lan.mu.Unlock()
	})
}

func healthLAN(t *testing.T, daemon testDaemon, remote string, headers http.Header) map[string]any {
	t.Helper()
	response := serve(t, daemon.handler, http.MethodGet, "/api/health", nil, remote, headers)
	if response.Code != http.StatusOK {
		t.Fatalf("health status = %d, body=%s", response.Code, response.Body.String())
	}
	var body map[string]any
	decodeBody(t, response, &body)
	lan, ok := body["lan"].(map[string]any)
	if !ok {
		t.Fatalf("health has no lan object: %#v", body)
	}
	return lan
}

// /api/health has to stay unauthenticated — discovery, the updater, and the
// frontend's origin bootstrap all read it — but the selected private IPv4 and
// port map the user's network for anyone who can reach the port.
func TestHealthRedactsTheLANURLFromUncredentialedPeers(t *testing.T) {
	daemon := newTestDaemon(t)
	fakeEnabledLAN(t, daemon)

	lan := healthLAN(t, daemon, "198.51.100.10:4321", nil)
	if lan["url"] != nil {
		t.Fatalf("unauthenticated peer saw lan.url = %v", lan["url"])
	}
	// The 200 and the enabled flag are what first-party probes actually read;
	// redaction must not take those away.
	if lan["enabled"] != true {
		t.Fatalf("lan.enabled = %v, want true so probes can still see the listener is up", lan["enabled"])
	}
	if bonjour, ok := lan["bonjour"].(map[string]any); !ok || bonjour["service"] == "" {
		t.Fatalf("lan.bonjour = %#v, want the discovery service name preserved", lan["bonjour"])
	}
}

func TestHealthKeepsTheLANURLForCallersThatBelongHere(t *testing.T) {
	daemon := newTestDaemon(t)
	fakeEnabledLAN(t, daemon)

	if lan := healthLAN(t, daemon, "127.0.0.1:5555", nil); lan["url"] != testLANURL {
		t.Errorf("loopback caller saw lan.url = %v, want %q", lan["url"], testLANURL)
	}
	authorized := http.Header{"Authorization": {"Bearer " + testToken}}
	if lan := healthLAN(t, daemon, "198.51.100.10:4321", authorized); lan["url"] != testLANURL {
		t.Errorf("token-bearing remote caller saw lan.url = %v, want %q", lan["url"], testLANURL)
	}
	badToken := http.Header{"Authorization": {"Bearer not-the-token"}}
	if lan := healthLAN(t, daemon, "198.51.100.10:4321", badToken); lan["url"] != nil {
		t.Errorf("a bad token still revealed lan.url = %v", lan["url"])
	}
}

// bootstrapCurrentOriginServer and `sessions lan` verification only look at the
// status code and the daemon's identity, so redaction must not disturb either.
func TestHealthStaysAnUnauthenticated200(t *testing.T) {
	daemon := newTestDaemon(t)
	fakeEnabledLAN(t, daemon)
	response := serve(t, daemon.handler, http.MethodGet, "/api/health", nil, "198.51.100.10:4321", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200; the frontend distinguishes 200 from 401 here", response.Code)
	}
	var body map[string]any
	decodeBody(t, response, &body)
	if body["ok"] != true || body["name"] != "sessionsd" {
		t.Fatalf("health identity = %#v, want the daemon still identifiable", body)
	}
}

// The finding: err.Error() from authorized() was rendered to a caller that had
// not authorized yet, and that error wraps an os.PathError naming the token
// file — an absolute path spelling out the state directory and the account name.
func TestUnauthorizedCallerNeverSeesTheTokenPath(t *testing.T) {
	daemon := newTestDaemon(t)
	// Make the token unreadable and unwritable by turning its path into a
	// directory, the same shape as a permission failure on the real file.
	if err := os.Remove(daemon.config.TokenPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(daemon.config.TokenPath, 0o700); err != nil {
		t.Fatal(err)
	}

	response := serve(t, daemon.handler, http.MethodGet, "/api/sessions", nil, "198.51.100.10:4321",
		http.Header{"Authorization": {"Bearer whatever"}})
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, secret := range []string{daemon.config.TokenPath, daemon.config.StateRoot, daemon.root} {
		if strings.Contains(body, secret) {
			t.Errorf("unauthenticated 500 leaked %q:\n%s", secret, body)
		}
	}
	if strings.Contains(body, "is a directory") {
		t.Errorf("unauthenticated 500 leaked the underlying syscall error:\n%s", body)
	}
	for _, want := range []string{"auth token", "sessionsd log"} {
		if !strings.Contains(body, want) {
			t.Errorf("500 body = %q, want it to name the problem and the next action (%q)", body, want)
		}
	}
}

// The operator on the machine keeps the diagnosis they need. A loopback peer is
// already authorized before the token is read, so this exercises the branch
// directly rather than through a route.
func TestAuthorizationFailureDetailStaysAvailableOnThisMachine(t *testing.T) {
	daemon := newTestDaemon(t)
	if err := os.Remove(daemon.config.TokenPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(daemon.config.TokenPath, 0o700); err != nil {
		t.Fatal(err)
	}
	// Loopback is authorized without reading the token at all: no error, no
	// leak, and the local user is never locked out by an unreadable token.
	response := serve(t, daemon.handler, http.MethodGet, "/api/health/deep", nil, "127.0.0.1:5555", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("loopback deep health status = %d, body=%s", response.Code, response.Body.String())
	}
}
