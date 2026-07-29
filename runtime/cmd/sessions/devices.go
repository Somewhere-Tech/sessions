package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"
)

type pairedDevice struct {
	DeviceID   string    `json:"device_id"`
	Name       string    `json:"name"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

type pairedDevicesResponse struct {
	Devices []pairedDevice `json:"devices"`
}

func (a *app) cmdDevices(args []string) error {
	if len(args) == 0 {
		return a.listPairedDevices()
	}
	if len(args) == 2 && args[0] == "revoke" {
		return a.revokePairedDevice(args[1])
	}
	return fail(1, "usage: sessions devices [revoke <id-or-prefix>]")
}

func (a *app) listPairedDevices() error {
	devices, err := a.fetchPairedDevices()
	if err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, pairedDevicesResponse{Devices: devices}, true)
	}
	if len(devices) == 0 {
		_, err := fmt.Fprintln(a.stdout, "No paired devices.")
		return err
	}
	writer := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "ID\tNAME\tCREATED\tLAST USED"); err != nil {
		return err
	}
	for _, device := range devices {
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n",
			prefixString(device.DeviceID, 8), displayDeviceName(device.Name),
			device.CreatedAt.Local().Format(time.RFC3339), device.LastUsedAt.Local().Format(time.RFC3339)); err != nil {
			return err
		}
	}
	return writer.Flush()
}

func (a *app) revokePairedDevice(prefix string) error {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return fail(1, "usage: sessions devices revoke <id-or-prefix>")
	}
	devices, err := a.fetchPairedDevices()
	if err != nil {
		return err
	}
	matches := make([]pairedDevice, 0, 1)
	for _, device := range devices {
		if strings.HasPrefix(device.DeviceID, prefix) {
			matches = append(matches, device)
		}
	}
	if len(matches) == 0 {
		return fail(1, "no paired device matches %q; run `sessions devices` to list device ids", prefix)
	}
	if len(matches) > 1 {
		return fail(1, "device prefix %q is ambiguous (%d matches); use more characters from `sessions devices`", prefix, len(matches))
	}
	matched := matches[0]
	response, err := a.api.request(context.Background(), http.MethodDelete, "/api/devices/"+escapeID(matched.DeviceID), nil, 5*time.Second)
	if err != nil {
		return fail(2, "cannot revoke %s (%s): %s", displayDeviceName(matched.Name), matched.DeviceID, err)
	}
	if response.status >= 400 {
		var payload struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(response.body, &payload) == nil && payload.Error != "" {
			return fail(2, "%s", payload.Error)
		}
		return fail(2, "/api/devices/%s returned HTTP %d", matched.DeviceID, response.status)
	}
	if a.wantJSON {
		return writeJSON(a.stdout, struct {
			Revoked bool         `json:"revoked"`
			Device  pairedDevice `json:"device"`
		}{true, matched}, true)
	}
	_, err = fmt.Fprintf(a.stdout, "Revoked %s (%s).\n", displayDeviceName(matched.Name), matched.DeviceID)
	return err
}

func (a *app) fetchPairedDevices() ([]pairedDevice, error) {
	response, err := a.api.request(context.Background(), http.MethodGet, "/api/devices", nil, 5*time.Second)
	if err != nil {
		return nil, fail(2, "cannot list paired devices: %s", err)
	}
	if response.status >= 400 {
		var payload struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(response.body, &payload) == nil && payload.Error != "" {
			return nil, fail(2, "%s", payload.Error)
		}
		return nil, fail(2, "/api/devices returned HTTP %d", response.status)
	}
	var listed pairedDevicesResponse
	if err := json.Unmarshal(response.body, &listed); err != nil {
		return nil, fail(2, "sessionsd returned an invalid device list: %s", err)
	}
	return listed.Devices, nil
}

func displayDeviceName(name string) string {
	return strings.Map(func(value rune) rune {
		if value == '\t' || value == '\n' || value == '\r' || value < 0x20 {
			return ' '
		}
		return value
	}, name)
}
