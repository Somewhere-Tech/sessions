package main

import (
	"fmt"
	"io"
)

type onboardingStatus struct {
	Version       int    `json:"version"`
	Complete      bool   `json:"complete"`
	RemoteControl string `json:"remoteControl"`
}

func (a *app) cmdOnboarding(args []string) error {
	if len(args) != 0 {
		return fail(1, "usage: sessions onboarding")
	}
	var status onboardingStatus
	if err := a.getJSON("/api/onboarding", &status); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, status, true)
	}
	completion := "pending"
	if status.Complete {
		completion = "complete"
	}
	if _, err := fmt.Fprintf(a.stdout, "Onboarding: %s\nClaude Remote Control: %s\n", completion, status.RemoteControl); err != nil {
		return err
	}
	if !status.Complete {
		_, err := io.WriteString(a.stdout, "Open Sessions.app to choose. The CLI can inspect this state but cannot grant consent.\n")
		return err
	}
	return nil
}
