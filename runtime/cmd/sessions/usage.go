package main

import (
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/somewhere-tech/sessions/runtime/internal/usage"
)

func (a *app) cmdUsage(args []string) error {
	group := "daily"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		group, args = strings.ToLower(args[0]), args[1:]
	}
	mode, hasMode := pluck(&args, "--mode")
	since, hasSince := pluck(&args, "--since")
	until, hasUntil := pluck(&args, "--until")
	provider, hasProvider := pluck(&args, "--provider")
	dimension, hasDimension := pluck(&args, "--dimension")
	if len(args) != 0 {
		return fail(1, "unknown usage option: %s\n%s", args[0], usageUsageText)
	}
	if !oneOfString(group, "daily", "weekly", "monthly", "session", "tag", "provider", "model") {
		return fail(1, "usage report must be daily, weekly, monthly, session, tag, provider, or model")
	}
	if !hasMode {
		mode = usage.ModeAuto
	}
	mode = strings.ToLower(mode)
	if !oneOfString(mode, usage.ModeAuto, usage.ModeCalculate, usage.ModeDisplay) {
		return fail(1, "--mode must be auto, calculate, or display")
	}
	provider = strings.ToLower(provider)
	if hasProvider && !oneOfString(provider, "claude", "codex") {
		return fail(1, "--provider must be claude or codex")
	}
	if group == "tag" && (!hasDimension || strings.TrimSpace(dimension) == "") {
		return fail(1, "tag reports need --dimension KEY")
	}
	parameters := url.Values{"group": {group}, "mode": {mode}}
	if hasSince {
		parameters.Set("since", since)
	}
	if hasUntil {
		parameters.Set("until", until)
	}
	if hasProvider {
		parameters.Set("provider", provider)
	}
	if hasDimension {
		parameters.Set("dimension", dimension)
	}
	var report usage.Report
	if err := a.getJSON("/api/usage?"+parameters.Encode(), &report); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, report, true)
	}
	return writeUsageTable(a.stdout, report)
}

func writeUsageTable(output io.Writer, report usage.Report) error {
	if len(report.Rows) == 0 {
		_, err := io.WriteString(output, "(no local usage found)\n")
		return err
	}
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "PERIOD\tINPUT\tOUTPUT\tREASONING\tCACHE WRITE\tCACHE READ\tTOTAL\tCOST")
	for _, row := range report.Rows {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t$%.4f\n", row.Key,
			commaInt(row.Tokens.Input), commaInt(row.Tokens.Output), commaInt(row.Tokens.Reasoning), commaInt(row.Tokens.CacheCreation),
			commaInt(row.Tokens.CacheRead), commaInt(row.Tokens.Total()), row.CostUSD)
	}
	fmt.Fprintf(writer, "TOTAL\t%s\t%s\t%s\t%s\t%s\t%s\t$%.4f\n", commaInt(report.Totals.Tokens.Input),
		commaInt(report.Totals.Tokens.Output), commaInt(report.Totals.Tokens.Reasoning), commaInt(report.Totals.Tokens.CacheCreation),
		commaInt(report.Totals.Tokens.CacheRead), commaInt(report.Totals.Tokens.Total()), report.Totals.CostUSD)
	if err := writer.Flush(); err != nil {
		return err
	}
	if report.Totals.MissingPricing > 0 {
		_, err := fmt.Fprintf(output, "\n%d usage entries have no price in the pinned ccusage snapshot; their calculated cost is $0.\n", report.Totals.MissingPricing)
		return err
	}
	return nil
}

func commaInt(value int64) string {
	raw := strconv.FormatInt(value, 10)
	start := 0
	if strings.HasPrefix(raw, "-") {
		start = 1
	}
	for index := len(raw) - 3; index > start; index -= 3 {
		raw = raw[:index] + "," + raw[index:]
	}
	return raw
}

func oneOfString(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if value == candidate {
			return true
		}
	}
	return false
}

const usageUsageText = "usage: sessions usage [daily|weekly|monthly|session|tag|provider|model] [--mode auto|calculate|display] [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--provider claude|codex] [--dimension KEY] [--json]"
