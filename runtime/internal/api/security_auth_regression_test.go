package api

import (
	"net/http"
	"testing"
)

func TestOriginlessLANWebsocketRequiresAuthenticationBeforeUpgrade(t *testing.T) {
	daemon := newTestDaemon(t)
	response := serve(t, daemon.handler, http.MethodGet, "/ws", nil, "192.168.1.25:54321", http.Header{
		"Connection": {"Upgrade"}, "Upgrade": {"websocket"},
		"Sec-Websocket-Version": {"13"}, "Sec-Websocket-Key": {"dGhlIHNhbXBsZSBub25jZQ=="},
	})
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated LAN upgrade: status %d, body %s", response.Code, response.Body.String())
	}
}
