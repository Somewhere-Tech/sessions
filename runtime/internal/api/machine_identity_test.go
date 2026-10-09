package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestMachineIdentityReturnsOnlyAuthenticatedPairingID(t *testing.T) {
	daemon := newTestDaemon(t)
	device, token, err := daemon.handler.pair.devices.create("MacBook")
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := daemon.handler.pair.devices.create("Other phone")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, peer, token, wantDevice string
		status                        int
	}{
		{"paired", "192.168.1.20:4321", token, device.DeviceID, 200},
		{"administrator", "192.168.1.20:4321", testToken, "", 200},
		{"local", "127.0.0.1:4321", token, "", 200},
		{"unauthenticated", "192.168.1.20:4321", "", "", 401},
	} {
		t.Run(test.name, func(t *testing.T) {
			headers := http.Header{"Authorization": []string{"Bearer " + test.token}}
			response := serve(t, daemon.handler, http.MethodGet, "/api/machine", nil, test.peer, headers)
			if response.Code != test.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var body struct {
				DeviceID string `json:"device_id"`
			}
			decodeBody(t, response, &body)
			if body.DeviceID != test.wantDevice || strings.Contains(response.Body.String(), other.DeviceID) || strings.Contains(response.Body.String(), token) {
				t.Fatalf("unexpected identity response: %s", response.Body.String())
			}
		})
	}
}
