package main

import (
	"fmt"
	"io"
	"strings"
)

type profileSession struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type profileStatus struct {
	Tool     string           `json:"tool"`
	Name     string           `json:"name"`
	Path     string           `json:"path"`
	Sessions []profileSession `json:"sessions"`
	LastUsed int64            `json:"last_used"`
}

func (a *app) cmdProfiles(args []string) error {
	if len(args) != 0 {
		return fail(1, "usage: sessions profiles (Sessions never deletes profile credentials; review the path from `sessions profiles` and remove it manually)")
	}
	var response struct {
		Profiles []profileStatus `json:"profiles"`
	}
	if err := a.getJSON("/api/profiles", &response); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, response.Profiles, true)
	}
	if len(response.Profiles) == 0 {
		_, err := io.WriteString(a.stdout, "(no profiles)\n")
		return err
	}
	rows := [][]string{{"TOOL", "NAME", "SESSIONS", "LAST-USED", "PATH"}}
	for _, profile := range response.Profiles {
		sessions := make([]string, 0, len(profile.Sessions))
		for _, current := range profile.Sessions {
			label := prefixString(current.ID, 8)
			if current.Name != "" {
				label = current.Name
			}
			sessions = append(sessions, label)
		}
		using := "-"
		if len(sessions) > 0 {
			using = strings.Join(sessions, ",")
		}
		lastUsed := "-"
		if profile.LastUsed > 0 {
			lastUsed = a.ageOf(profile.LastUsed)
		}
		rows = append(rows, []string{profile.Tool, profile.Name, using, lastUsed, profile.Path})
	}
	if err := writePaddedRows(a.stdout, rows); err != nil {
		return err
	}
	_, err := fmt.Fprintln(a.stdout, "Sessions never deletes profile credentials; remove one manually only after reviewing its PATH above.")
	return err
}
