package main

import (
	"fmt"
	"io"
)

type worktreeStatus struct {
	SessionID       string `json:"session"`
	SessionName     string `json:"session_name,omitempty"`
	WorktreePath    string `json:"worktree_path"`
	Branch          string `json:"branch"`
	Base            string `json:"base"`
	SourceRepo      string `json:"source_repo"`
	TreeState       string `json:"tree_state"`
	Dirty           bool   `json:"dirty"`
	MergedIntoBase  bool   `json:"merged_into_base"`
	SessionState    string `json:"session_state"`
	InspectionError string `json:"inspection_error,omitempty"`
	Exists          bool   `json:"exists"`
}

type worktreeCleanResult struct {
	worktreeStatus
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
}

func (a *app) cmdWorktrees(args []string) error {
	if len(args) == 0 {
		return a.listWorktrees()
	}
	if args[0] != "clean" {
		return fail(1, "usage: sessions worktrees [clean [--dry-run]]")
	}
	dryRun := false
	for _, argument := range args[1:] {
		if argument != "--dry-run" || dryRun {
			return fail(1, "usage: sessions worktrees clean [--dry-run]")
		}
		dryRun = true
	}
	var response struct {
		Results []worktreeCleanResult `json:"results"`
		DryRun  bool                  `json:"dry_run"`
	}
	if err := a.postJSON("/api/worktrees/clean", map[string]bool{"dry_run": dryRun}, &response, 2); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, response, true)
	}
	if len(response.Results) == 0 {
		_, err := io.WriteString(a.stdout, "(no Sessions-created worktrees)\n")
		return err
	}
	for _, result := range response.Results {
		switch result.Action {
		case "would-remove":
			if _, err := fmt.Fprintf(a.stdout, "would remove %s (%s)\n", result.WorktreePath, result.Branch); err != nil {
				return err
			}
		case "removed":
			if _, err := fmt.Fprintf(a.stdout, "removed %s (%s)\n", result.WorktreePath, result.Branch); err != nil {
				return err
			}
		case "removed-worktree":
			if _, err := fmt.Fprintf(a.stdout, "removed worktree %s but kept branch %s: %s\n", result.WorktreePath, result.Branch, result.Reason); err != nil {
				return err
			}
		default:
			if _, err := fmt.Fprintf(a.stdout, "skip %s: %s\n", result.WorktreePath, result.Reason); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *app) listWorktrees() error {
	var response struct {
		Worktrees []worktreeStatus `json:"worktrees"`
	}
	if err := a.getJSON("/api/worktrees", &response); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, response.Worktrees, true)
	}
	if len(response.Worktrees) == 0 {
		_, err := io.WriteString(a.stdout, "(no Sessions-created worktrees)\n")
		return err
	}
	rows := [][]string{{"SESSION", "BRANCH", "TREE", "MERGED", "STATE", "PATH"}}
	for _, worktree := range response.Worktrees {
		session := worktree.SessionName
		if session == "" {
			session = prefixString(worktree.SessionID, 8)
		}
		merged := "no"
		if worktree.MergedIntoBase {
			merged = "yes"
		}
		tree := worktree.TreeState
		if worktree.InspectionError != "" {
			tree = "error"
		}
		rows = append(rows, []string{session, worktree.Branch, tree, merged, worktree.SessionState, worktree.WorktreePath})
	}
	return writePaddedRows(a.stdout, rows)
}
