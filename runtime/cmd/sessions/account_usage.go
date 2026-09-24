package main

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

type accountUsageWindow struct {
	Kind          string `json:"kind"`
	UsedPercent   int    `json:"used_percent"`
	WindowMinutes *int64 `json:"window_minutes,omitempty"`
	ResetsAt      *int64 `json:"resets_at,omitempty"`
}

type accountUsageBucket struct {
	LimitID   string               `json:"limit_id,omitempty"`
	LimitName string               `json:"limit_name,omitempty"`
	Plan      string               `json:"plan,omitempty"`
	Reached   string               `json:"reached,omitempty"`
	Windows   []accountUsageWindow `json:"windows"`
	Credits   *struct {
		HasCredits bool   `json:"has_credits"`
		Unlimited  bool   `json:"unlimited"`
		Balance    string `json:"balance,omitempty"`
	} `json:"credits,omitempty"`
}

type accountUsage struct {
	Tool      string               `json:"tool"`
	Name      string               `json:"name"`
	Label     string               `json:"label,omitempty"`
	State     string               `json:"state"`
	Message   string               `json:"message,omitempty"`
	CheckedAt int64                `json:"checked_at,omitempty"`
	Identity  *profileIdentity     `json:"identity,omitempty"`
	Buckets   []accountUsageBucket `json:"buckets,omitempty"`
	ReadAt    int64                `json:"read_at,omitempty"`
	Stale     bool                 `json:"stale,omitempty"`
}

// cmdAccountsUsage is `sessions accounts usage`: each account's allowance as
// its provider reports it on this computer, or on --machine.
func (a *app) cmdAccountsUsage(args []string) error {
	tool, _ := pluck(&args, "--tool")
	refresh := removeFirst(&args, "--refresh")
	if len(args) > 1 || (tool != "" && tool != "claude" && tool != "codex") || (len(args) == 1 && tool == "") {
		return fail(1, "usage: sessions accounts usage [<name> --tool claude|codex] [--tool claude|codex] [--refresh]")
	}
	query := url.Values{}
	if tool != "" {
		query.Set("tool", tool)
	}
	if len(args) == 1 {
		query.Set("name", args[0])
	}
	if refresh {
		query.Set("refresh", "1")
	}
	path := "/api/account-usage"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var response struct {
		Accounts []accountUsage `json:"accounts"`
	}
	if err := a.getJSON(path, &response); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, response.Accounts, true)
	}
	return a.writeAccountUsage(response.Accounts)
}

func (a *app) writeAccountUsage(accounts []accountUsage) error {
	if len(accounts) == 0 {
		_, err := fmt.Fprintln(a.stdout, "(no accounts; `sessions accounts add` adds one)")
		return err
	}
	rows := [][]string{{"ACCOUNT", "LABEL", "STATE", "LIMIT", "WINDOW", "USED", "RESETS", "READ"}}
	var notes []string
	for _, account := range accounts {
		name, label := account.Tool+"/"+account.Name, orDash(account.Label)
		state := account.State
		if account.Stale {
			state += " (stale)"
		}
		read := "-"
		if account.ReadAt > 0 {
			read = a.ageOf(account.ReadAt)
		}
		if account.Message != "" {
			notes = append(notes, name+": "+account.Message)
		}
		lines := usageLines(account.Buckets)
		if len(lines) == 0 {
			rows = append(rows, []string{name, label, state, "-", "-", "-", "-", read})
		}
		for _, line := range lines {
			rows = append(rows, append([]string{name, label, state}, append(line, read)...))
		}
	}
	if err := writePaddedRows(a.stdout, rows); err != nil {
		return err
	}
	for _, note := range notes {
		if _, err := fmt.Fprintln(a.stdout, note); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(a.stdout, "Each limit is separate and is never added to another. A stale reading is the last one the provider gave.")
	return err
}

// usageLines is one row per window: LIMIT, WINDOW, USED, RESETS.
func usageLines(buckets []accountUsageBucket) [][]string {
	var lines [][]string
	for _, bucket := range buckets {
		limit := orDash(bucket.LimitName)
		if limit == "-" {
			limit = orDash(bucket.LimitID)
		}
		for _, window := range bucket.Windows {
			span, resets := window.Kind, "-"
			if window.WindowMinutes != nil {
				span = (time.Duration(*window.WindowMinutes) * time.Minute).String()
			}
			if window.ResetsAt != nil {
				resets = time.UnixMilli(*window.ResetsAt).Local().Format("Jan 2 15:04")
			}
			lines = append(lines, []string{limit, span, fmt.Sprintf("%d%%", window.UsedPercent), resets})
		}
		if bucket.Reached != "" {
			lines = append(lines, []string{limit, "-", "limit reached", "-"})
		}
	}
	return lines
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
