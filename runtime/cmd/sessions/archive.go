package main

import (
	"fmt"
	"strings"
)

func (a *app) cmdArchive(args []string) error {
	if len(args) == 0 {
		return fail(1, "usage: sessions archive <session> [session...]")
	}
	ids := make([]string, 0, len(args))
	seen := make(map[string]struct{}, len(args))
	for _, value := range args {
		if strings.HasPrefix(value, "-") {
			return fail(1, "usage: sessions archive <session> [session...]")
		}
		id, err := a.resolveSessionID(value)
		if err != nil {
			return err
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	var result retentionResult
	if err := a.postJSON("/api/retention/archive", map[string]any{"ids": ids}, &result, 2); err != nil {
		return err
	}
	archived := 0
	for _, item := range result.Items {
		if item.Status == "archived" {
			archived++
		}
	}
	if a.wantJSON {
		if err := writeJSON(a.stdout, result, true); err != nil {
			return err
		}
		if archived == 0 {
			return fail(2, "no selected records were archived")
		}
		return nil
	}
	for _, item := range result.Items {
		if item.Status == "archived" {
			fmt.Fprintf(a.stdout, "archived  %s  %s\n", prefixString(item.ID, 8), strings.TrimSpace(item.Name))
			continue
		}
		fmt.Fprintf(a.stdout, "skipped   %s  %s\n", prefixString(item.ID, 8), item.Reason)
	}
	if archived == 0 {
		return fail(2, "no selected records were archived")
	}
	_, err := fmt.Fprintf(a.stdout, "\nArchived %d closed record(s). History and transcripts were preserved.\n", archived)
	return err
}
