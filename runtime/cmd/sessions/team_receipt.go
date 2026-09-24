package main

import (
	"fmt"
	"strings"
)

type teamHandoff struct {
	Source    string   `json:"source"`
	Seq       uint64   `json:"seq,omitempty"`
	At        string   `json:"at,omitempty"`
	Outcome   string   `json:"outcome,omitempty"`
	Summary   string   `json:"summary,omitempty"`
	Workspace string   `json:"workspace,omitempty"`
	Branch    string   `json:"branch,omitempty"`
	Commits   []string `json:"commits,omitempty"`
	Push      string   `json:"push"`
	Tests     []string `json:"tests,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
	Remaining []string `json:"remaining,omitempty"`
	Detail    string   `json:"detail,omitempty"`
}

type teamCheckout struct {
	Path     string   `json:"path,omitempty"`
	Dirty    *bool    `json:"dirty,omitempty"`
	Sessions []string `json:"sessions,omitempty"`
	Detail   string   `json:"detail"`
}

func (a *app) writeTeamChangesFooter(listing teamListing) error {
	for _, member := range listing.Members {
		if member.Checkout != nil {
			if _, err := fmt.Fprintf(a.stdout, "\n%s: %s\n", shortID(member.ID), member.Checkout.Detail); err != nil {
				return err
			}
		}
		if receipt := member.Handoff; receipt != nil && receipt.Source == "agent-reported" {
			if _, err := fmt.Fprintf(a.stdout, "\n%s handoff (agent-reported): %s — %s\n  workspace: %s; branch: %s; push: %s\n",
				shortID(member.ID), receipt.Outcome, receipt.Summary, receipt.Workspace, receipt.Branch, receipt.Push); err != nil {
				return err
			}
			for _, section := range []struct {
				label  string
				values []string
			}{
				{"commit", receipt.Commits}, {"test", receipt.Tests}, {"artifact", receipt.Artifacts}, {"remaining", receipt.Remaining},
			} {
				for _, value := range section.values {
					if _, err := fmt.Fprintf(a.stdout, "  %s: %s\n", section.label, value); err != nil {
						return err
					}
				}
			}
		}
		if member.Handoff != nil && member.Handoff.Detail != "" {
			if _, err := fmt.Fprintf(a.stdout, "\n%s: %s\n", shortID(member.ID), member.Handoff.Detail); err != nil {
				return err
			}
		}
	}
	for _, id := range listing.Removed {
		if _, err := fmt.Fprintf(a.stdout, "\n%s is no longer in this team's listing (not proof of completion).\n", id); err != nil {
			return err
		}
	}
	if listing.Delta {
		if _, err := fmt.Fprintf(a.stdout, "\n%d changed, %d removed; %d members total, %d need input.\n", len(listing.Members), len(listing.Removed), listing.Total, listing.NeedsInput); err != nil {
			return err
		}
	}
	if listing.NextCursor != "" {
		_, err := fmt.Fprintf(a.stdout, "\nNext check: sessions team %s--since %s --json\n", strings.TrimSpace(listing.Caller)+" ", listing.NextCursor)
		return err
	}
	return nil
}
