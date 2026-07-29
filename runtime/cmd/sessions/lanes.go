package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/waitcond"
)

const laneExitKind waitcond.Kind = "lane_exit"

const defaultLaneWaitTimeout = 30 * time.Second

type laneManifest struct {
	ExitCode       int     `json:"exit_code"`
	Signal         *string `json:"signal"`
	DurationMS     int64   `json:"duration_ms"`
	LastOutputTail string  `json:"last_output_tail"`
	SpecPath       string  `json:"spec_path"`
	FilesChanged   *int    `json:"files_changed,omitempty"`
}

type laneView struct {
	session
	Kind     string        `json:"kind"`
	SpecPath string        `json:"specPath,omitempty"`
	Manifest *laneManifest `json:"manifest,omitempty"`
}

type lanesResponse struct {
	Lanes         []laneView `json:"lanes"`
	UserCreatorID string     `json:"user_creator_id"`
}

func (a *app) cmdLSDispatch(args []string) error {
	kind, present := pluck(&args, "--kind")
	if !present {
		return a.cmdLS(args)
	}
	if kind != "lane" {
		return fail(1, "unsupported --kind %q (valid: lane)", kind)
	}
	return a.cmdLanes(args)
}

func (a *app) cmdLanes(args []string) error {
	options, err := parseLaneListOptions(args)
	if err != nil {
		return err
	}
	var response lanesResponse
	if err := a.getJSON("/api/lanes", &response); err != nil {
		return err
	}
	response.Lanes, err = filterLaneViews(response.Lanes, options, response.UserCreatorID)
	if err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, response.Lanes, true)
	}
	if options.mine && options.owner == "" && os.Getenv("SESSIONS_OWNER_ID") == "" && os.Getenv("SESSIONS_SESSION_ID") == "" {
		userID := response.UserCreatorID
		if userID == "" {
			userID, err = ledger.LocalUserCreatorID()
			if err != nil {
				return err
			}
		}
		writeOSUserScope(a.stdout, ownershipScope{kind: "user", id: userID, osUserFallback: true})
	}
	if len(response.Lanes) == 0 {
		_, err := io.WriteString(a.stdout, "(no lanes)\n")
		return err
	}
	rows := [][]string{{"ID", "NAME", "DESC", "TOOL", "CWD", "STATE", "EXIT", "DURATION", "PROVENANCE"}}
	for _, lane := range response.Lanes {
		name := "-"
		if strings.TrimSpace(lane.Name) != "" {
			name = strings.Join(strings.Fields(lane.Name), " ")
		}
		state := "running"
		exit := "-"
		duration := "-"
		if lane.Manifest != nil {
			state = "exited"
			exit = strconv.Itoa(lane.Manifest.ExitCode)
			if lane.Manifest.Signal != nil && *lane.Manifest.Signal != "" {
				exit += "/" + *lane.Manifest.Signal
			}
			duration = formatLaneDuration(lane.Manifest.DurationMS)
		} else if lane.Exited {
			state = "exited"
			if lane.ExitCode != nil {
				exit = strconv.Itoa(*lane.ExitCode)
			}
			if lane.ExitSignal != nil && *lane.ExitSignal != "" {
				exit += "/" + *lane.ExitSignal
			}
		}
		rows = append(rows, []string{
			prefixString(lane.ID, 8), name, compactDescription(lane.Description), toolOfSession(lane.session),
			strings.Replace(lane.Cwd, a.home, "~", 1), state, exit, duration, laneProvenanceLabel(lane),
		})
	}
	return writePaddedRows(a.stdout, rows)
}

type laneListOptions struct {
	all           bool
	mine          bool
	direct        bool
	detach        bool
	subtree       string
	owner         string
	explicitOwner bool
}

func parseLaneListOptions(args []string) (laneListOptions, error) {
	options := laneListOptions{}
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "-a", "--include-exited":
			// Lanes have always included retained exited entries; preserve this
			// compatibility spelling as a no-op.
		case "--all":
			options.all = true
		case "--mine":
			options.mine = true
		case "--direct":
			options.direct = true
		case "--detach":
			options.detach = true
		case "--owner", "--subtree":
			if index+1 >= len(args) || strings.TrimSpace(args[index+1]) == "" || strings.HasPrefix(args[index+1], "--") {
				return options, fail(1, "%s needs a non-empty id", args[index])
			}
			value := strings.TrimSpace(args[index+1])
			if args[index] == "--owner" {
				options.owner = value
				options.explicitOwner = true
				options.mine = true
			} else {
				options.subtree = value
			}
			index++
		default:
			return options, fail(1, "usage: sessions lanes [--all | --mine [--owner ID] | --subtree ID] [--direct] [--detach]")
		}
	}
	if options.all && (options.mine || options.subtree != "" || options.direct || options.detach) {
		return options, fail(1, "--all cannot be combined with provenance selectors")
	}
	if options.mine && options.subtree != "" {
		return options, fail(1, "--mine and --subtree cannot be combined")
	}
	if options.direct && !options.mine && options.subtree == "" {
		return options, fail(1, "--direct requires --mine or --subtree")
	}
	if options.detach && !options.explicitOwner {
		return options, fail(1, "--detach requires an explicit --owner")
	}
	if options.explicitOwner && strings.TrimSpace(os.Getenv("SESSIONS_SESSION_ID")) != "" && !options.detach {
		return options, fail(1, "--owner conflicts with inherited SESSIONS_SESSION_ID; pass --detach to select an external root")
	}
	return options, nil
}

func filterLaneViews(lanes []laneView, options laneListOptions, daemonUserID string) ([]laneView, error) {
	if options.all || (!options.mine && options.subtree == "") {
		return lanes, nil
	}
	kind, id := "session", options.subtree
	if options.mine {
		ownerEnvironment := os.Getenv("SESSIONS_OWNER_ID")
		if ownerEnvironment != "" && strings.TrimSpace(ownerEnvironment) != ownerEnvironment {
			return nil, fail(1, "SESSIONS_OWNER_ID must not contain surrounding whitespace")
		}
		sessionEnvironment := os.Getenv("SESSIONS_SESSION_ID")
		if sessionEnvironment != "" && strings.TrimSpace(sessionEnvironment) != sessionEnvironment {
			return nil, fail(1, "SESSIONS_SESSION_ID must not contain surrounding whitespace")
		}
		switch {
		case options.owner != "":
			kind, id = "external", options.owner
		case ownerEnvironment != "":
			kind, id = "external", ownerEnvironment
		case sessionEnvironment != "":
			if !looksLikeLaneID(sessionEnvironment) {
				return nil, fail(1, "SESSIONS_SESSION_ID is not a session UUID")
			}
			kind, id = "session", sessionEnvironment
		default:
			id = daemonUserID
			if id == "" {
				// Compatibility with older daemons which predate the principal hint.
				resolvedID, resolveErr := ledger.LocalUserCreatorID()
				if resolveErr != nil {
					return nil, resolveErr
				}
				id = resolvedID
			}
			kind = "user"
		}
	}
	if kind == "session" && options.subtree != "" {
		resolved, err := resolveSubtreeID(lanes, id)
		if err != nil {
			return nil, err
		}
		id = resolved
	}
	if options.direct && kind != "session" {
		return nil, fail(1, "--direct applies only to session ancestry")
	}
	scope := ownershipScope{kind: kind, id: id}
	filtered := make([]laneView, 0, len(lanes))
	for _, lane := range lanes {
		if matchesOwnership(lane.session, scope, options.direct) {
			filtered = append(filtered, lane)
		}
	}
	return filtered, nil
}

func resolveSubtreeID(lanes []laneView, idOrPrefix string) (string, error) {
	candidates := make(map[string]struct{})
	for _, lane := range lanes {
		candidates[lane.ID] = struct{}{}
		for _, ancestor := range lane.CreatorAncestry {
			candidates[ancestor] = struct{}{}
		}
	}
	if _, exact := candidates[idOrPrefix]; exact {
		return idOrPrefix, nil
	}
	matches := make([]string, 0, 2)
	for candidate := range candidates {
		if strings.HasPrefix(candidate, idOrPrefix) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return "", fail(1, "ambiguous subtree prefix %q", idOrPrefix)
	}
	if looksLikeLaneID(idOrPrefix) {
		// A valid session can legitimately have no lane descendants, in which
		// case it will not otherwise appear in the lane-only response.
		return idOrPrefix, nil
	}
	return "", fail(1, "no session ancestry matching %q", idOrPrefix)
}

func laneProvenanceLabel(lane laneView) string {
	if lane.ProvenanceStatus != "" {
		return lane.ProvenanceStatus
	}
	return "-"
}

func formatLaneDuration(milliseconds int64) string {
	if milliseconds < 1000 {
		return fmt.Sprintf("%dms", milliseconds)
	}
	return fmt.Sprintf("%.1fs", float64(milliseconds)/1000)
}

func (a *app) cmdLastDispatch(args []string) error {
	if len(args) == 0 || args[0] == "" {
		return a.cmdLast(args)
	}
	id, lane, err := a.resolveLaneID(args[0])
	if err != nil {
		return err
	}
	if !lane {
		return a.cmdLast(args)
	}
	if len(args) != 1 {
		return fail(1, "usage: sessions last <lane-id> [--json]")
	}
	manifest, statusCode, err := a.fetchLaneManifest(context.Background(), id)
	if err != nil {
		return err
	}
	if statusCode == http.StatusConflict {
		return fail(1, "lane %s is still running", id)
	}
	if statusCode != http.StatusOK {
		return fail(1, "no completion manifest for lane %s", id)
	}
	if a.wantJSON {
		return writeJSON(a.stdout, manifest, true)
	}
	_, err = io.WriteString(a.stdout, manifest.LastOutputTail)
	if err == nil && !strings.HasSuffix(manifest.LastOutputTail, "\n") {
		_, err = io.WriteString(a.stdout, "\n")
	}
	return err
}

func (a *app) cmdWaitDispatch(args []string) error {
	if hasWaitCondition(args) {
		return a.cmdWait(args)
	}
	ids, any, summary, timeout, parseErr := parseLaneWaitArgs(args)
	if parseErr != nil {
		return a.cmdWait(args)
	}
	resolved := make([]string, 0, len(ids))
	for _, candidate := range ids {
		id, lane, err := a.resolveLaneID(candidate)
		if err != nil {
			return err
		}
		if !lane {
			if len(ids) == 1 && !any {
				return a.cmdWait(args)
			}
			return fail(1, "%s is not a lane", candidate)
		}
		resolved = append(resolved, id)
	}
	if len(resolved) > 1 && !any {
		return fail(1, "multiple lanes require --any")
	}
	completedID, manifest, err := a.waitForLaneExit(resolved, timeout)
	if err != nil {
		return err
	}
	return a.writeLaneWaitCompletion(completedID, manifest, false, summary)
}

func (a *app) waitForLaneExit(ids []string, timeout time.Duration) (string, laneManifest, error) {
	conditions := make([]waitcond.Condition, 0, len(ids))
	for _, id := range ids {
		conditions = append(conditions, &laneExitCondition{app: a, id: id})
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	started := time.Now()
	result, err := waitcond.WaitAny(ctx, conditions)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "", laneManifest{}, a.writeWaitTimeout(timeout, time.Since(started), len(conditions))
		}
		return "", laneManifest{}, fail(1, "%s", err)
	}
	manifest, statusCode, err := a.fetchLaneManifest(context.Background(), result.Session)
	if err != nil || statusCode != http.StatusOK {
		if err == nil {
			err = fmt.Errorf("completion manifest returned HTTP %d", statusCode)
		}
		return "", laneManifest{}, fail(1, "%s", err)
	}
	return result.Session, manifest, nil
}

func (a *app) writeLaneWaitCompletion(id string, manifest laneManifest, outputOnly, includeSummary bool) error {
	if a.wantJSON {
		output := struct {
			ID      string `json:"id"`
			Summary string `json:"summary,omitempty"`
			laneManifest
		}{ID: id, Summary: compactSummary(manifest.LastOutputTail), laneManifest: manifest}
		if !includeSummary {
			output.Summary = ""
		}
		if err := writeJSON(a.stdout, output, false); err != nil {
			return err
		}
	} else if outputOnly {
		if err := writeLaneOutputTail(a.stdout, manifest.LastOutputTail); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(a.stdout, "%s exited %d after %s\n", id, manifest.ExitCode, formatLaneDuration(manifest.DurationMS))
		if includeSummary {
			fmt.Fprintf(a.stdout, "summary: %s\n", compactSummary(manifest.LastOutputTail))
		}
	}
	if manifest.ExitCode != 0 {
		return status(manifest.ExitCode)
	}
	return nil
}

func writeLaneOutputTail(writer io.Writer, output string) error {
	if _, err := io.WriteString(writer, output); err != nil {
		return err
	}
	if !strings.HasSuffix(output, "\n") {
		_, err := io.WriteString(writer, "\n")
		return err
	}
	return nil
}

func hasWaitCondition(args []string) bool {
	for _, argument := range args {
		switch argument {
		case "--until", "--until-file-contains", "--until-idle-stable":
			return true
		}
	}
	return false
}

func parseLaneWaitArgs(args []string) ([]string, bool, bool, time.Duration, error) {
	ids := make([]string, 0, 2)
	any := false
	summary := false
	timeout := defaultLaneWaitTimeout
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--any":
			if any {
				return nil, false, false, 0, errors.New("duplicate --any")
			}
			any = true
		case "--summary":
			if summary {
				return nil, false, false, 0, errors.New("duplicate --summary")
			}
			summary = true
		case "--timeout":
			if index+1 >= len(args) {
				return nil, false, false, 0, errors.New("missing timeout")
			}
			index++
			parsed, err := parseDuration(args[index], 0)
			if err != nil || parsed <= 0 {
				return nil, false, false, 0, errors.New("invalid timeout")
			}
			timeout = parsed
		case "--idle":
			if index+1 >= len(args) {
				return nil, false, false, 0, errors.New("missing idle duration")
			}
			index++
			if parsed, err := parseDuration(args[index], 0); err != nil || parsed < 0 {
				return nil, false, false, 0, errors.New("invalid idle duration")
			}
		default:
			if strings.HasPrefix(args[index], "-") || args[index] == "" {
				return nil, false, false, 0, errors.New("not a lane wait")
			}
			ids = append(ids, args[index])
		}
	}
	if len(ids) == 0 {
		return nil, false, false, 0, errors.New("no lanes")
	}
	return ids, any, summary, timeout, nil
}

type laneExitCondition struct {
	app *app
	id  string
}

func (condition *laneExitCondition) Wait(ctx context.Context) (waitcond.Result, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, statusCode, err := condition.app.fetchLaneManifest(ctx, condition.id)
		if err != nil {
			return waitcond.Result{}, err
		}
		if statusCode == http.StatusOK {
			return waitcond.Result{Kind: laneExitKind, Session: condition.id}, nil
		}
		if statusCode != http.StatusConflict {
			return waitcond.Result{}, fmt.Errorf("lane %s is unavailable", condition.id)
		}
		select {
		case <-ctx.Done():
			return waitcond.Result{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (a *app) resolveLaneID(idOrPrefix string) (string, bool, error) {
	var response lanesResponse
	listed, err := a.api.request(context.Background(), http.MethodGet, "/api/lanes", nil, 0)
	if err != nil {
		return "", false, err
	}
	if listed.status == http.StatusNotFound {
		return "", false, nil
	}
	if listed.status >= 400 {
		return "", false, fail(2, "/api/lanes → %d %s", listed.status, prefixBytes(listed.body, 200))
	}
	if err := json.Unmarshal(listed.body, &response); err != nil {
		return "", false, err
	}
	for _, lane := range response.Lanes {
		if lane.ID == idOrPrefix {
			return lane.ID, true, nil
		}
	}
	matches := make([]string, 0, 2)
	for _, lane := range response.Lanes {
		if strings.HasPrefix(lane.ID, idOrPrefix) {
			matches = append(matches, lane.ID)
		}
	}
	if len(matches) == 1 {
		return matches[0], true, nil
	}
	if len(matches) > 1 {
		return "", false, fail(1, "ambiguous lane prefix %q", idOrPrefix)
	}
	if looksLikeLaneID(idOrPrefix) {
		_, statusCode, err := a.fetchLaneManifest(context.Background(), idOrPrefix)
		if err != nil {
			return "", false, err
		}
		if statusCode == http.StatusOK || statusCode == http.StatusConflict {
			return idOrPrefix, true, nil
		}
	}
	return "", false, nil
}

func (a *app) fetchLaneManifest(ctx context.Context, id string) (laneManifest, int, error) {
	response, err := a.api.request(ctx, http.MethodGet, "/api/lanes/"+escapeID(id)+"/manifest", nil, 0)
	if err != nil {
		return laneManifest{}, 0, err
	}
	var manifest laneManifest
	if response.status == http.StatusOK {
		if err := json.Unmarshal(response.body, &manifest); err != nil {
			return laneManifest{}, response.status, err
		}
	}
	return manifest, response.status, nil
}

func looksLikeLaneID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for index, value := range id {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if value != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", value) {
			return false
		}
	}
	return true
}
