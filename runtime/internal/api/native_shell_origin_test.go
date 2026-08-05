package api

import "testing"

// The Tauri webview reports a different origin per platform: macOS uses the
// custom tauri:// scheme (whose hostname is "localhost", so the generic loopback
// checks happened to accept it) while Windows and Android report
// tauri.localhost, which is not a loopback hostname.
//
// trustedAmbientWriteOrigin has always trusted all three, but allowedOrigin
// recognized only the macOS spelling, so the native client's WebSocket upgrade
// was refused outright on Windows and Android. The two predicates now share one
// source of truth; this test pins that they agree about the native shell.
func TestNativeShellOriginsMayUpgradeAndWriteOnEveryPlatform(t *testing.T) {
	for origin := range nativeShellOrigins {
		if !allowedOrigin(origin, "127.0.0.1") {
			t.Errorf("allowedOrigin(%q) = false; the native client cannot open /ws", origin)
		}
		if !trustedAmbientWriteOrigin(origin, "127.0.0.1", 8787) {
			t.Errorf("trustedAmbientWriteOrigin(%q) = false; the native client cannot write", origin)
		}
	}

	for _, origin := range []string{"tauri://localhost", "http://tauri.localhost", "https://tauri.localhost"} {
		if _, listed := nativeShellOrigins[origin]; !listed {
			t.Errorf("%q is missing from nativeShellOrigins", origin)
		}
	}

	// Narrowing check: a lookalike host must not inherit native-shell trust.
	for _, origin := range []string{
		"http://tauri.localhost.evil.test",
		"http://nottauri.localhost",
		"http://tauri.localhost:3000",
	} {
		if trustedAmbientWriteOrigin(origin, "127.0.0.1", 8787) {
			t.Errorf("trustedAmbientWriteOrigin(%q) = true, want false", origin)
		}
	}
}
