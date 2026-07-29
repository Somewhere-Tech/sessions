package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type notifySettings struct {
	Done    bool `json:"done"`
	Waiting bool `json:"waiting"`
	Lost    bool `json:"lost"`
}

type notifyStatus struct {
	Notify     notifySettings `json:"notify"`
	Subscribed bool           `json:"subscribed"`
}

func (a *app) cmdNotify(args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return fail(1, "usage: sessions notify <status|on|off> [done|waiting|lost]")
	}
	action := args[0]
	kind := ""
	if len(args) == 2 {
		kind = args[1]
		if !validNotifyKind(kind) {
			return fail(1, "unknown notification kind %q; choose done, waiting, or lost", kind)
		}
	}
	var (
		current notifyStatus
		err     error
	)
	switch action {
	case "status":
		current, err = a.requestNotify(http.MethodGet, nil, action)
	case "on", "off":
		current, err = a.requestNotify(http.MethodPost, map[string]any{
			"enabled": action == "on", "kind": kind,
		}, action)
	default:
		return fail(1, "unknown notify action %q; choose status, on, or off", action)
	}
	if err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, current, false)
	}
	if action != "status" {
		target := kind
		if target == "" {
			target = "done, waiting, and lost"
		}
		state := "enabled"
		if action == "off" {
			state = "disabled"
		}
		if _, err := fmt.Fprintf(a.stdout, "Notifications %s for %s.\n", state, target); err != nil {
			return err
		}
	}
	return a.printNotifyStatus(current, kind)
}

func validNotifyKind(kind string) bool {
	return kind == "done" || kind == "waiting" || kind == "lost"
}

func (a *app) requestNotify(method string, body any, action string) (notifyStatus, error) {
	response, err := a.api.request(context.Background(), method, "/api/notify", body, 5*time.Second)
	if err != nil {
		return notifyStatus{}, fail(2, "cannot reach sessionsd: %s. Start it with `sessions install`, then retry `sessions notify %s`.", err, action)
	}
	if response.status >= 400 {
		var payload struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(response.body, &payload) == nil && payload.Error != "" {
			return notifyStatus{}, fail(2, "%s", payload.Error)
		}
		return notifyStatus{}, fail(2, "/api/notify returned HTTP %d: %s", response.status, prefixBytes(response.body, 200))
	}
	var current notifyStatus
	if err := json.Unmarshal(response.body, &current); err != nil {
		return notifyStatus{}, fail(2, "sessionsd returned an invalid notification status: %s", err)
	}
	return current, nil
}

func (a *app) printNotifyStatus(current notifyStatus, selected string) error {
	values := []struct {
		name    string
		enabled bool
	}{
		{name: "done", enabled: current.Notify.Done},
		{name: "waiting", enabled: current.Notify.Waiting},
		{name: "lost", enabled: current.Notify.Lost},
	}
	for _, value := range values {
		if selected != "" && selected != value.name {
			continue
		}
		state := "off"
		if value.enabled {
			state = "on"
		}
		if _, err := fmt.Fprintf(a.stdout, "%s: %s\n", value.name, state); err != nil {
			return err
		}
	}
	if current.Subscribed {
		_, err := io.WriteString(a.stdout, "Push subscription: active.\n")
		return err
	}
	_, err := io.WriteString(a.stdout, "Push subscription: none. Subscribe in the Sessions web UI to opt in.\n")
	return err
}
