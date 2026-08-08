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
	// The obvious thing to try, given that this is the command that reports
	// what a session spent. Point at the command that answers it instead of
	// refusing with "unknown option", which leaves the caller to guess whether
	// the capability exists at all.
	for _, argument := range args {
		if argument == "--resources" || argument == "--resource" {
			return fail(1, "usage reports tokens and cost over a period; machine memory and CPU are a live measurement with no history. Run `sessions resources`.")
		}
	}
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
	fmt.Fprintln(writer, "PERIOD\tINPUT\tOUTPUT\tREASONING\tCACHE WRITE\tCACHE READ\tTOTAL\tEST COST")
	for _, row := range report.Rows {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", row.Key,
			commaInt(row.Tokens.Input), commaInt(row.Tokens.Output), commaInt(row.Tokens.Reasoning), commaInt(row.Tokens.CacheCreation),
			commaInt(row.Tokens.CacheRead), commaInt(row.Tokens.Total()), formatEstimatedCost(row.CostUSD))
	}
	fmt.Fprintf(writer, "TOTAL\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", commaInt(report.Totals.Tokens.Input),
		commaInt(report.Totals.Tokens.Output), commaInt(report.Totals.Tokens.Reasoning), commaInt(report.Totals.Tokens.CacheCreation),
		commaInt(report.Totals.Tokens.CacheRead), commaInt(report.Totals.Tokens.Total()), formatEstimatedCost(report.Totals.CostUSD))
	if err := writer.Flush(); err != nil {
		return err
	}
	if _, err := io.WriteString(output, "\n"+estimatedCostDisclosure(report)); err != nil {
		return err
	}
	if report.Totals.MissingPricing > 0 {
		_, err := fmt.Fprintf(output, "%d usage entries have no price in the pinned ccusage snapshot; their calculated cost is $0.\n", report.Totals.MissingPricing)
		return err
	}
	return nil
}

// formatEstimatedCost renders a modelled figure at a precision it can actually
// support. Four decimals read as a settled amount to the tenth of a cent, which
// this number is not: it is reconstructed from a token stream that omits
// server-side tool use and from prices pinned in this build. Cents are the
// honest resolution, and anything that rounds away is reported as such rather
// than shown as a clean $0.00.
func formatEstimatedCost(value float64) string {
	if value == 0 {
		return "$0.00"
	}
	if value < 0 {
		return "-" + formatEstimatedCost(-value)
	}
	if value < 0.005 {
		return "<$0.01"
	}
	rounded := fmt.Sprintf("%.2f", value)
	dollars, cents, _ := strings.Cut(rounded, ".")
	whole, err := strconv.ParseInt(dollars, 10, 64)
	if err != nil {
		return "$" + rounded
	}
	return "$" + commaInt(whole) + "." + cents
}

// estimatedCostDisclosure states, on the surface that prints the number, what
// the audit of internal/usage established. The JSON report already carries
// report.Pricing.Note; a caller reading the table never saw it.
func estimatedCostDisclosure(report usage.Report) string {
	lines := []string{
		"EST COST is a modelled estimate, not a bill: it answers \"what would this have cost on the API\".",
		"Prices are pinned in this build with no as-of date, server-side tool use is billed but never appears",
		"in the token stream, and 1-hour cache writes are underpriced. On a Max or ChatGPT subscription the",
		"marginal cost of this usage is zero.",
	}
	if note := strings.TrimSpace(report.Pricing.Note); note != "" {
		lines = append(lines, "Pricing source: "+note)
	}
	return strings.Join(lines, "\n") + "\n"
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
