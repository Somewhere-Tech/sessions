package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type asideItem struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Name   string `json:"name,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type asideResult struct {
	Items []asideItem `json:"items"`
}

func (a *app) cmdAside(args []string) error {
	clear := false
	targets := make([]string, 0, len(args))
	for _, arg := range args {
		switch {
		case arg == "--clear":
			clear = true
		case strings.HasPrefix(arg, "-"):
			return fail(1, "usage: sessions aside <session> [session...] [--clear]")
		default:
			targets = append(targets, arg)
		}
	}
	if len(targets) == 0 {
		return fail(1, "usage: sessions aside <session> [session...] [--clear]")
	}

	ids := make([]string, 0, len(targets))
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		id, err := a.resolveSessionID(target)
		if err != nil {
			return err
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	result := asideResult{Items: make([]asideItem, 0, len(ids))}
	changed := 0
	for _, id := range ids {
		path := "/api/sessions/" + escapeID(id) + "/set-aside"
		response, err := a.api.request(context.Background(), http.MethodPut, path, map[string]any{"setAside": !clear}, 0)
		if err != nil {
			return fail(2, "%s → %s", path, err)
		}
		if response.status >= 400 {
			var payload struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(response.body, &payload)
			if response.status == http.StatusConflict && payload.Error != "" {
				result.Items = append(result.Items, asideItem{ID: id, Status: "skipped", Reason: payload.Error})
				continue
			}
			if payload.Error != "" {
				return fail(2, "%s", payload.Error)
			}
			return fail(2, "%s → %d %s", path, response.status, prefixBytes(response.body, 200))
		}
		current, resolveErr := a.resolveStatusSession(id)
		name := ""
		if resolveErr == nil {
			name = strings.TrimSpace(current.Name)
		}
		status := "set-aside"
		if clear {
			status = "brought-back"
		}
		result.Items = append(result.Items, asideItem{ID: id, Status: status, Name: name})
		changed++
	}

	if a.wantJSON {
		if err := writeJSON(a.stdout, result, true); err != nil {
			return err
		}
	} else {
		for _, item := range result.Items {
			if item.Status == "skipped" {
				fmt.Fprintf(a.stdout, "skipped       %s  %s\n", prefixString(item.ID, 8), item.Reason)
				continue
			}
			fmt.Fprintf(a.stdout, "%-13s %s  %s\n", item.Status, prefixString(item.ID, 8), item.Name)
		}
	}
	if changed == 0 {
		return fail(2, "no live sessions changed working-set state")
	}
	return nil
}
