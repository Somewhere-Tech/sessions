package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

const (
	defaultRetentionAge = 30 * 24 * time.Hour
	maximumRetentionAge = 10 * 365 * 24 * time.Hour
)

type retentionItem struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Kind       string `json:"kind"`
	ClosedAtMS int64  `json:"closed_at_ms"`
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
}

type retentionResult struct {
	DryRun   bool            `json:"dry_run"`
	CutoffMS int64           `json:"cutoff_ms"`
	Items    []retentionItem `json:"items"`
}

func (a *app) cmdGC(args []string) error {
	apply := removeFirst(&args, "--apply")
	dryRun := removeFirst(&args, "--dry-run")
	if apply && dryRun {
		return fail(1, "--apply and --dry-run cannot be combined")
	}
	olderThan := defaultRetentionAge
	if value, present := pluck(&args, "--older-than"); present {
		parsed, err := parseRetentionAge(value)
		if err != nil {
			return err
		}
		olderThan = parsed
	}
	if len(args) != 0 {
		return fail(1, "usage: sessions gc [--older-than DURATION] [--apply]")
	}
	var result retentionResult
	if err := a.postJSON("/api/retention/gc", map[string]any{
		"older_than_ms": olderThan.Milliseconds(),
		"dry_run":       !apply,
	}, &result, 2); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, result, true)
	}
	targets, skipped := 0, 0
	for _, item := range result.Items {
		switch item.Status {
		case "archived", "would_archive":
			targets++
			name := strings.TrimSpace(item.Name)
			if name == "" {
				name = "-"
			}
			fmt.Fprintf(a.stdout, "%-13s %s  %s  %s\n", item.Status, prefixString(item.ID, 8), item.Kind, name)
		default:
			skipped++
		}
	}
	if targets == 0 {
		_, err := io.WriteString(a.stdout, "No eligible closed records.\n")
		return err
	}
	if !apply {
		fmt.Fprintf(a.stdout,
			"\nDry run: %d record(s) would be archived; %d skipped. Recovery history and transcripts are preserved.\nRun `sessions gc --older-than %s --apply` to archive them.\n",
			targets, skipped, formatRetentionAge(olderThan))
		return nil
	}
	fmt.Fprintf(a.stdout,
		"\nArchived %d closed record(s); %d skipped. Recovery history and transcripts were preserved.\n",
		targets, skipped)
	return nil
}

func parseRetentionAge(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if strings.HasSuffix(value, "d") {
		days, err := strconv.ParseInt(strings.TrimSuffix(value, "d"), 10, 32)
		if err != nil || days <= 0 || days > 10*365 {
			return 0, fail(1, "invalid --older-than %q (examples: 24h, 7d, 30d)", value)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < time.Hour || duration > maximumRetentionAge {
		return 0, fail(1, "invalid --older-than %q (range 1h to 3650d; examples: 24h, 7d, 30d)", value)
	}
	return duration, nil
}

func formatRetentionAge(value time.Duration) string {
	if value%(24*time.Hour) == 0 {
		return strconv.FormatInt(int64(value/(24*time.Hour)), 10) + "d"
	}
	return value.String()
}
