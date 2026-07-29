package main

import (
	"fmt"
	"strings"
)

type providerStatus struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Installed       bool   `json:"installed"`
	Version         string `json:"version,omitempty"`
	LatestVersion   string `json:"latestVersion,omitempty"`
	LastCheckedAt   string `json:"lastCheckedAt,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable"`
}

func (a *app) cmdProviders(args []string) error {
	if len(args) >= 1 && args[0] == "update" {
		if len(args) != 2 || (args[1] != "claude" && args[1] != "codex") {
			return fail(1, "usage: sessions providers update claude|codex")
		}
		var result struct {
			Provider providerStatus `json:"provider"`
			Output   string         `json:"output"`
		}
		if err := a.postJSON("/api/providers/"+args[1]+"/update", map[string]any{}, &result, 2); err != nil {
			return err
		}
		if a.wantJSON {
			return writeJSON(a.stdout, result, true)
		}
		version := result.Provider.Version
		if version == "" {
			version = "unknown"
		}
		_, err := fmt.Fprintf(a.stdout, "%s updated to %s. Running sessions keep their existing process; new sessions use this version.\n",
			result.Provider.Name, version)
		return err
	}
	if len(args) != 0 {
		return fail(1, "usage: sessions providers [update claude|codex]")
	}
	var result struct {
		Providers []providerStatus `json:"providers"`
	}
	if err := a.getJSON("/api/providers", &result); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, result, true)
	}
	for _, provider := range result.Providers {
		status := "not installed"
		if provider.Installed {
			status = provider.Version
			if provider.UpdateAvailable {
				status += " · update available: " + provider.LatestVersion
			}
		}
		fmt.Fprintf(a.stdout, "%-12s %s\n", provider.Name, strings.TrimSpace(status))
	}
	return nil
}
