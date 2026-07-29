package main

import (
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
)

func (a *app) cmdRecover(args []string) error {
	reopen := removeFirst(&args, "--reopen")
	force := removeFirst(&args, "--force")
	all := removeFirst(&args, "--all")
	if len(args) != 0 {
		return fail(1, "usage: sessions recover [--all | --reopen [--force]]")
	}
	if force && !reopen {
		return fail(1, "--force requires --reopen")
	}
	if all && reopen {
		return fail(1, "--all cannot be combined with --reopen")
	}
	if reopen {
		var result recovery.ReopenResult
		if err := a.postJSON("/api/recovery/reopen", map[string]any{"force": force}, &result, 2); err != nil {
			return err
		}
		if a.wantJSON {
			if err := writeJSON(a.stdout, result, true); err != nil {
				return err
			}
		} else if err := writeReopenResult(a, result); err != nil {
			return err
		}
		if !result.OK {
			return status(1)
		}
		return nil
	}

	var report recovery.Report
	if err := a.getJSON("/api/recovery", &report); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, recoveryJSONView(report), true)
	}
	return writeRecoveryPlan(a, report, all)
}

type recoveryLaneJSON struct {
	recovery.Lane
	Status string `json:"status"`
}

type recoveryReportJSON struct {
	GeneratedAtMS int64               `json:"generatedAtMs"`
	Lanes         []recoveryLaneJSON  `json:"lanes"`
	Plan          ledger.RecoveryPlan `json:"plan"`
}

func recoveryJSONView(report recovery.Report) recoveryReportJSON {
	recipes := recoveryRecipesByLane(report)
	lanes := make([]recoveryLaneJSON, 0, len(report.Lanes))
	for _, lane := range report.Lanes {
		status, _ := recoveryStatusReason(lane, recipes[lane.ID])
		lanes = append(lanes, recoveryLaneJSON{Lane: lane, Status: status})
	}
	return recoveryReportJSON{GeneratedAtMS: report.GeneratedAtMS, Lanes: lanes, Plan: report.Plan}
}

func recoveryRecipesByLane(report recovery.Report) map[string]ledger.RecoveryRecipe {
	recipes := make(map[string]ledger.RecoveryRecipe, len(report.Plan.Recipes))
	for _, recipe := range report.Plan.Recipes {
		recipes[recipe.SourceLaneID] = recipe
	}
	return recipes
}

func writeRecoveryPlan(a *app, report recovery.Report, all bool) error {
	recipes := recoveryRecipesByLane(report)
	w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	header := "NAME\tTOOL\tCWD\tLAST-ACTIVITY\tRESUME"
	if all {
		header = "NAME\tTOOL\tCWD\tLAST-ACTIVITY\tSTATUS\tREASON"
	}
	if _, err := fmt.Fprintln(w, header); err != nil {
		return err
	}
	count := 0
	for _, lane := range report.Lanes {
		if lane.Class != ledger.ClassUnexpectedlyLost {
			continue
		}
		recipe, hasRecipe := recipes[lane.ID]
		if !all && (!hasRecipe || recipe.Blocked) {
			continue
		}
		name := lane.Name
		if name == "" {
			name = lane.ID
		}
		count++
		if all {
			status, reason := recoveryStatusReason(lane, recipe)
			if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				name, lane.Tool, lane.Cwd, recoveryTime(lane.LastActivityAtMS), status, reason); err != nil {
				return err
			}
			continue
		}
		resume := shellRecipe(append([]string{recipe.Cmd}, recipe.Args...))
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			name, lane.Tool, lane.Cwd, recoveryTime(lane.LastActivityAtMS), resume); err != nil {
			return err
		}
	}
	if count == 0 {
		message := "(no actionable recoveries)\t\t\t\t"
		if all {
			message = "(no unexpectedly-lost lanes)\t\t\t\t\t"
		}
		if _, err := fmt.Fprintln(w, message); err != nil {
			return err
		}
	}
	return w.Flush()
}

func recoveryStatusReason(lane recovery.Lane, recipe ledger.RecoveryRecipe) (string, string) {
	if lane.Class != ledger.ClassUnexpectedlyLost {
		return string(lane.Class), "not unexpectedly lost"
	}
	if recipe.SourceLaneID != "" {
		if recipe.Blocked {
			return "blocked", "resume source is stale or missing"
		}
		return "actionable", "resume with " + shellRecipe(append([]string{recipe.Cmd}, recipe.Args...))
	}
	for _, anomaly := range lane.Anomalies {
		if anomaly == ledger.AnomalyProviderUnbound {
			return "provider-unbound", "provider did not bind; no safe resume recipe"
		}
	}
	return "unresumable", "no safe resume recipe"
}

func writeReopenResult(a *app, result recovery.ReopenResult) error {
	if len(result.Outcomes) == 0 {
		_, err := fmt.Fprintln(a.stdout, "no unexpectedly-lost lanes")
		return err
	}
	for _, outcome := range result.Outcomes {
		name := outcome.Name
		if name == "" {
			name = outcome.SourceLaneID
		}
		line := fmt.Sprintf("%s: %s", name, outcome.Status)
		if outcome.NewLaneID != "" {
			line += " " + outcome.NewLaneID
		}
		if outcome.Error != "" {
			line += " (" + outcome.Error + ")"
		}
		if _, err := fmt.Fprintln(a.stdout, line); err != nil {
			return err
		}
	}
	return nil
}

func recoveryTime(atMS int64) string {
	if atMS == 0 {
		return "-"
	}
	return time.UnixMilli(atMS).Format(time.RFC3339)
}

func shellRecipe(argv []string) string {
	quoted := make([]string, 0, len(argv))
	for _, value := range argv {
		if value != "" && !strings.ContainsAny(value, " \t\n\"'\\$`!&;|<>()[]{}*?") {
			quoted = append(quoted, value)
		} else {
			quoted = append(quoted, strconv.Quote(value))
		}
	}
	return strings.Join(quoted, " ")
}
