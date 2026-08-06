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

// Recovery statuses a lost record can report. transcript-recovery is the one
// that had no name before: the provider deleted its own transcript, so the
// stored resume argv is a command the provider will refuse, but Sessions kept
// its own copy of the conversation and can still bring it back.
const (
	recoveryStatusActionable         = "actionable"
	recoveryStatusTranscriptRecovery = "transcript-recovery"
	recoveryStatusBlocked            = "blocked"
	recoveryStatusProviderUnbound    = "provider-unbound"
	recoveryStatusUnresumable        = "unresumable"
)

// laneRecovery is the single answer every recover branch produces, so the
// default table, the --all explanation, and the --json document cannot
// disagree about how a conversation comes back. status names the branch,
// reason says it in prose, and argv is the command that actually works -- or
// nothing, when nothing does. Nothing else may be printed as a recovery
// command, which is what kept a provider resume flag in front of users whose
// provider had already deleted the transcript it names.
type laneRecovery struct {
	status             string
	reason             string
	argv               []string
	transcriptRecovery bool
}

type recoveryLaneJSON struct {
	recovery.Lane
	Status string `json:"status"`
	// Reason is the same sentence `recover --all` prints, so a JSON caller
	// never has to re-derive an explanation from the status alone.
	Reason string `json:"reason"`
	// TranscriptRecovery marks a conversation that comes back from Sessions'
	// own copy. Native provider resume is impossible for it, and `recover`
	// carries `sessions resume` rather than the provider's resume argv.
	TranscriptRecovery bool `json:"transcriptRecovery,omitempty"`
	// Recover is the argv that recovers this conversation, absent when no
	// command can.
	Recover []string `json:"recover,omitempty"`
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
		outcome := recoveryOutcome(lane, recipes[lane.ID])
		lanes = append(lanes, recoveryLaneJSON{
			Lane:               lane,
			Status:             outcome.status,
			Reason:             outcome.reason,
			TranscriptRecovery: outcome.transcriptRecovery,
			Recover:            outcome.argv,
		})
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
	fromMirror := 0
	for _, lane := range report.Lanes {
		if lane.Class != ledger.ClassUnexpectedlyLost {
			continue
		}
		outcome := recoveryOutcome(lane, recipes[lane.ID])
		// The default view lists what a caller can act on. That set is
		// decided by whether a recovery command exists at all, not by
		// recipe.Blocked, so a conversation recoverable only from Sessions'
		// copy stays listed and keeps its own honest command.
		if !all && len(outcome.argv) == 0 {
			continue
		}
		name := lane.Name
		if name == "" {
			name = lane.ID
		}
		count++
		if outcome.transcriptRecovery {
			fromMirror++
		}
		if all {
			if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				name, lane.Tool, lane.Cwd, recoveryTime(lane.LastActivityAtMS),
				outcome.status, outcome.reason); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			name, lane.Tool, lane.Cwd, recoveryTime(lane.LastActivityAtMS),
			shellRecipe(outcome.argv)); err != nil {
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
	if err := w.Flush(); err != nil {
		return err
	}
	if fromMirror > 0 {
		// Without this the table reads as if every row comes back the same
		// way. These do not: the provider no longer holds the conversation,
		// so the command in the RESUME column is a Sessions command and a
		// native provider resume would be refused.
		if _, err := fmt.Fprintf(a.stdout,
			"\n%s recoverable only from Sessions' own copy: the provider deleted its transcript, "+
				"so a native provider resume would be refused. Use the `sessions resume` command "+
				"shown, which replays Sessions' copy into a new session.\n",
			pluralConversations(fromMirror, "is", "are")); err != nil {
			return err
		}
	}
	return nil
}

func recoveryOutcome(lane recovery.Lane, recipe ledger.RecoveryRecipe) laneRecovery {
	if lane.Class != ledger.ClassUnexpectedlyLost {
		return laneRecovery{status: string(lane.Class), reason: "not unexpectedly lost"}
	}
	if recipe.SourceLaneID != "" {
		switch {
		case recipe.TranscriptRecovery:
			// recipe.Cmd still names the provider resume that was recorded
			// when the conversation was created, and the provider will refuse
			// it now that its own transcript is gone. Printing it would send
			// the user to a command that fails on a conversation Sessions can
			// still bring back, so the recipe is replaced rather than shown.
			argv := []string{"sessions", "resume", lane.ID}
			return laneRecovery{
				status:             recoveryStatusTranscriptRecovery,
				reason:             "provider transcript is gone; recover from Sessions' copy with " + shellRecipe(argv),
				argv:               argv,
				transcriptRecovery: true,
			}
		case recipe.Blocked:
			return laneRecovery{
				status: recoveryStatusBlocked,
				reason: "resume source is stale or missing, and Sessions has no copy",
			}
		}
		argv := append([]string{recipe.Cmd}, recipe.Args...)
		return laneRecovery{
			status: recoveryStatusActionable,
			reason: "resume with " + shellRecipe(argv),
			argv:   argv,
		}
	}
	for _, anomaly := range lane.Anomalies {
		if anomaly == ledger.AnomalyProviderUnbound {
			return laneRecovery{
				status: recoveryStatusProviderUnbound,
				reason: "provider did not bind; no safe resume recipe",
			}
		}
	}
	return laneRecovery{status: recoveryStatusUnresumable, reason: "no safe resume recipe"}
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
