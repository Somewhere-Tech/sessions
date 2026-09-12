package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	verdictprotocol "github.com/somewhere-tech/sessions/runtime/internal/verdict"
)

type gitStatus struct {
	Branch     string `json:"branch"`
	Head       string `json:"head"`
	DirtyCount int    `json:"dirty_count"`
}

type statusOutput struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Description       string `json:"description"`
	DescriptionSource string `json:"description_source,omitempty"`
	// Record says what this document describes. `kind` is left to the session
	// record below, where it means the session's own kind, exactly as
	// `sessions ls --json` reports it.
	Record            string                   `json:"record"`
	Tool              string                   `json:"tool"`
	State             string                   `json:"state"`
	ExitCode          *int                     `json:"exit_code,omitempty"`
	Cwd               string                   `json:"cwd"`
	Profile           string                   `json:"profile,omitempty"`
	ConfigDir         string                   `json:"config_dir,omitempty"`
	WorktreePath      string                   `json:"worktree_path,omitempty"`
	Branch            string                   `json:"branch,omitempty"`
	Base              string                   `json:"base,omitempty"`
	SourceRepo        string                   `json:"source_repo,omitempty"`
	Git               *gitStatus               `json:"git"`
	LastVerdict       *verdictprotocol.Summary `json:"last_verdict,omitempty"`
	LastActivityAt    string                   `json:"last_activity_at"`
	CreatedAt         string                   `json:"created_at"`
	AgeMS             int64                    `json:"age_ms"`
	IdleReason        string                   `json:"idle_reason,omitempty"`
	IdleDetail        string                   `json:"idle_detail,omitempty"`
	IdleSinceMS       *int64                   `json:"idle_since_ms,omitempty"`
	LastSummary       string                   `json:"last_summary,omitempty"`
	RunnerProtocol    int                      `json:"runner_protocol"`
	RunnerVersion     string                   `json:"runner_version,omitempty"`
	EndedByKind       string                   `json:"ended_by_kind,omitempty"`
	EndedByID         string                   `json:"ended_by_id,omitempty"`
	EndedByName       string                   `json:"ended_by_name,omitempty"`
	EndedByClient     string                   `json:"ended_by_client,omitempty"`
	EndReason         string                   `json:"end_reason,omitempty"`
	EndOperationID    string                   `json:"end_operation_id,omitempty"`
	SetAsideAtMS      *int64                   `json:"set_aside_at_ms,omitempty"`
	Permissions       string                   `json:"permissions,omitempty"`
	Lifecycle         string                   `json:"lifecycle,omitempty"`
	Unreachable       bool                     `json:"unreachable,omitempty"`
	UnreachableReason string                   `json:"unreachable_reason,omitempty"`
	UnreachableSince  *int64                   `json:"unreachable_since_ms,omitempty"`
	RunnerGone        bool                     `json:"runner_gone,omitempty"`
	// gitMissing is not part of the document: `git` is already null when there
	// are no facts, and a caller has nothing different to do about the reason.
	// The card says it because a person on a fresh machine does.
	gitMissing bool
}

func (a *app) cmdStatus(args []string) error {
	if len(args) != 1 || args[0] == "" {
		return fail(1, "usage: sessions status <id> [--json]")
	}
	record, err := a.resolveStatusRecord(args[0])
	if err != nil {
		return err
	}
	current := &record.value
	id := current.ID

	git, gitMissing := inspectGit(current.Cwd)
	latest, err := a.latestVerdict(id)
	if err != nil {
		return err
	}

	now := a.now()
	createdAt := current.CreatedAt
	lastActivityAt := current.LastDataAt
	if lastActivityAt < createdAt {
		lastActivityAt = createdAt
	}
	if current.LastUserMessageAt != nil && *current.LastUserMessageAt > lastActivityAt {
		lastActivityAt = *current.LastUserMessageAt
	}
	lastActivityTime := time.UnixMilli(lastActivityAt).UTC()
	var summary *verdictprotocol.Summary
	if latest != nil {
		value := latest.Summary()
		summary = &value
		if emittedAt, parseErr := time.Parse(time.RFC3339Nano, latest.EmittedAt); parseErr == nil && emittedAt.After(lastActivityTime) {
			lastActivityTime = emittedAt
			lastActivityAt = emittedAt.UnixMilli()
		}
	}
	state := liveStatusState(*current)
	output := statusOutput{
		ID: id, Name: current.Name, Description: current.Description,
		DescriptionSource: current.DescriptionSource, Record: "session", Tool: toolOfSession(*current),
		State: state, Cwd: current.Cwd, Profile: current.Profile, ConfigDir: current.ConfigDir,
		WorktreePath: current.WorktreePath, Branch: current.Branch, Base: current.Base, SourceRepo: current.SourceRepo,
		Git: git, gitMissing: gitMissing, LastVerdict: summary,
		LastActivityAt: lastActivityTime.Format(time.RFC3339Nano),
		CreatedAt:      formatStatusTime(createdAt),
		AgeMS:          max(now.UnixMilli()-createdAt, 0),
		IdleReason:     current.IdleReason, IdleDetail: current.IdleDetail,
		IdleSinceMS: current.IdleSince, LastSummary: current.LastSummary,
		RunnerProtocol: current.RunnerProtocol, RunnerVersion: current.RunnerVersion,
		EndedByKind: current.EndedByKind, EndedByID: current.EndedByID,
		EndedByName: current.EndedByName, EndedByClient: current.EndedByClient, EndReason: current.EndReason,
		EndOperationID: current.EndOperationID,
		SetAsideAtMS:   current.SetAsideAt,
		Permissions:    current.Permissions, Lifecycle: current.Lifecycle,
		Unreachable: current.Unreachable, UnreachableReason: current.UnreachableReason,
		UnreachableSince: current.UnreachableSince, RunnerGone: current.RunnerGone,
	}
	if current.Exited {
		output.ExitCode = current.ExitCode
	}
	if a.wantJSON {
		document, mergeErr := statusDocument(record.raw, output)
		if mergeErr != nil {
			return mergeErr
		}
		return writeJSON(a.stdout, document, true)
	}
	return a.writeStatusCard(output, *current, lastActivityAt)
}

// liveStatusState describes the runtime that exists now. IdleReason describes
// the last turn. Keeping those axes separate prevents a completed turn from
// making a reusable, live session look terminal to a person or a watcher.
func liveStatusState(current session) string {
	if current.Exited {
		return "exited"
	}
	if current.RunnerGone {
		return "lost"
	}
	if current.Unreachable {
		if current.UnreachableReason == "restart-restore-pending" {
			return "needs-recovery"
		}
		return "unreachable"
	}
	if current.SetAsideAt != nil {
		return "set-aside"
	}
	if current.Working {
		return "working"
	}
	if current.IdleReason == "needs-input" {
		return "needs-you"
	}
	return "idle"
}

// statusDocument is the session record `sessions ls --json` returns, with what
// status knows on top of it.
//
// An agent that inspects one session before sending to it read a thinner truth
// than one that listed everything: status answered with its own hand-built
// shape, so working, exited and failureKind were simply absent and "idle" could
// not be told from "field not present". The listing record is now the base, and
// it wins every name it defines, so the two commands cannot drift.
func statusDocument(raw json.RawMessage, output statusOutput) (map[string]json.RawMessage, error) {
	document := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &document); err != nil {
			return nil, err
		}
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return nil, err
	}
	statusOnly := map[string]json.RawMessage{}
	if err := json.Unmarshal(encoded, &statusOnly); err != nil {
		return nil, err
	}
	for key, value := range statusOnly {
		if _, listed := document[key]; listed {
			continue
		}
		document[key] = value
	}
	// The one value status derives rather than reports: a daemon that sends no
	// tool for a session leaves the listing's field empty, and status has
	// always answered with the tool its command implies.
	if string(document["tool"]) == `""` {
		document["tool"] = statusOnly["tool"]
	}
	return document, nil
}

func (a *app) resolveStatusRecord(idOrPrefix string) (sessionRecord, error) {
	deadline := a.now().Add(startupWaitBudget)
	for {
		records, err := a.fetchSessionRecords(true)
		if err != nil {
			return sessionRecord{}, err
		}
		sessions := make([]session, 0, len(records))
		for _, record := range records {
			sessions = append(sessions, record.value)
		}
		candidates := candidatesForSessions(a, sessions)
		id, found, resolveErr := resolveIDPrefix(idOrPrefix, "session", "sessions ls", candidates)
		if resolveErr != nil {
			return sessionRecord{}, resolveErr
		}
		if found {
			return records[candidateIndex(id, candidates)], nil
		}
		// A daemon that is still loading has not reached this session yet.
		// Saying it does not exist is the answer that sent a teammate looking
		// for a lane that was running the whole time.
		if !a.waitForLoadingDaemon(deadline) {
			return sessionRecord{}, fail(1, "%s", unknownSessionMessage(idOrPrefix))
		}
	}
}

func (a *app) resolveStatusSession(idOrPrefix string) (*session, error) {
	sessions, err := a.listSessions(true)
	if err != nil {
		return nil, err
	}
	candidates := candidatesForSessions(a, sessions)
	id, found, resolveErr := resolveIDPrefix(idOrPrefix, "session", "sessions ls", candidates)
	if resolveErr != nil {
		return nil, resolveErr
	}
	if !found {
		return nil, fail(1, "%s", unknownSessionMessage(idOrPrefix))
	}
	return &sessions[candidateIndex(id, candidates)], nil
}

func (a *app) latestVerdict(id string) (*verdictprotocol.Record, error) {
	path := "/api/sessions/" + escapeID(id) + "/verdict"
	response, err := a.api.request(context.Background(), "GET", path, nil, 0)
	if err != nil {
		return nil, err
	}
	if response.status == 404 {
		return nil, nil
	}
	if response.status >= 400 {
		return nil, fail(2, "%s → %d %s", path, response.status, prefixBytes(response.body, 200))
	}
	var record verdictprotocol.Record
	if err := json.Unmarshal(response.body, &record); err != nil {
		return nil, err
	}
	return &record, nil
}

// inspectGit reads the git facts status decorates a session with. They are
// decoration: a machine without git still has sessions, and a status command
// that refuses to answer because an optional tool is missing has turned a
// missing nicety into a broken verb. Verified on a Linux container, where
// `sessions status <id>` failed outright with
// `inspect git in /: exec: "git": executable file not found in $PATH`.
//
// Every outcome that is not "here are the facts" returns no facts and no error.
// missing reports the one case worth saying out loud, so the card can say why
// the line is empty rather than leaving a bare dash.
func inspectGit(cwd string) (status *gitStatus, missing bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := exec.LookPath("git"); err != nil {
		return nil, true
	}
	probe := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--is-inside-work-tree")
	probeOutput, err := probe.Output()
	if err != nil {
		return nil, false
	}
	if strings.TrimSpace(string(probeOutput)) != "true" {
		return nil, false
	}
	rootOutput, err := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil, false
	}
	root := strings.TrimSpace(string(rootOutput))
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		absHome, homeAbsErr := filepath.Abs(home)
		absCWD, cwdAbsErr := filepath.Abs(cwd)
		absRoot, rootAbsErr := filepath.Abs(root)
		if homeAbsErr == nil && cwdAbsErr == nil && rootAbsErr == nil && absHome == absRoot && absCWD == absRoot {
			return nil, false
		}
	}
	command := exec.CommandContext(ctx, "git", "-C", cwd, "status", "--porcelain=v2", "--branch", "--untracked-files=normal", "--", ".")
	encoded, err := command.Output()
	if err != nil {
		return nil, false
	}
	result := &gitStatus{}
	for _, line := range strings.Split(strings.TrimSuffix(string(encoded), "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "# branch.oid "):
			result.Head = strings.TrimPrefix(line, "# branch.oid ")
		case strings.HasPrefix(line, "# branch.head "):
			result.Branch = strings.TrimPrefix(line, "# branch.head ")
		case line != "" && !strings.HasPrefix(line, "# "):
			result.DirtyCount++
		}
	}
	if result.Head == "(initial)" {
		result.Head = ""
	}
	return result, false
}

func formatStatusTime(milliseconds int64) string {
	return time.UnixMilli(milliseconds).UTC().Format(time.RFC3339Nano)
}

func (a *app) writeStatusCard(output statusOutput, current session, lastActivityAt int64) error {
	label := output.Name
	if label == "" {
		label = prefixString(output.ID, 8)
	}
	if _, err := fmt.Fprintf(a.stdout, "%s  %s\n", label, output.State); err != nil {
		return err
	}
	kind := current.Kind
	if kind == "" {
		kind = "session"
	}
	if _, err := fmt.Fprintf(a.stdout, "  id       %s\n  kind     %s\n  tool     %s\n  cwd      %s\n",
		output.ID, kind, output.Tool, a.homeRelative(output.Cwd)); err != nil {
		return err
	}
	if err := writeStatusStateLines(a.stdout, current); err != nil {
		return err
	}
	description := output.Description
	if description == "" {
		description = "-"
	}
	if _, err := fmt.Fprintf(a.stdout, "  desc     %s\n", description); err != nil {
		return err
	}
	if output.Profile != "" {
		if _, err := fmt.Fprintf(a.stdout, "  profile  %s\n  config   %s\n", output.Profile,
			a.homeRelative(output.ConfigDir)); err != nil {
			return err
		}
	}
	if output.WorktreePath != "" {
		if _, err := fmt.Fprintf(a.stdout, "  worktree %s\n  branch   %s\n  base     %s\n  source   %s\n",
			a.homeRelative(output.WorktreePath), output.Branch, output.Base,
			a.homeRelative(output.SourceRepo)); err != nil {
			return err
		}
	}
	if output.SetAsideAtMS != nil {
		label := "was set aside"
		detail := ""
		if output.State == "set-aside" {
			label = "set aside"
			detail = " — hidden from the Sessions working set; the runtime is running"
		}
		if _, err := fmt.Fprintf(a.stdout, "  %-9s %s ago%s\n",
			label, a.ageOf(*output.SetAsideAtMS), detail); err != nil {
			return err
		}
	}
	if output.ExitCode != nil {
		if _, err := fmt.Fprintf(a.stdout, "  exit     %d\n", *output.ExitCode); err != nil {
			return err
		}
	}
	if output.Unreachable {
		detail := "unreachable — " + output.UnreachableReason
		if output.UnreachableReason == "restart-restore-pending" {
			detail = "unreachable — paused after reboot; run `sessions resume " + output.ID + "`"
		}
		if _, err := fmt.Fprintf(a.stdout, "  recovery %s\n", detail); err != nil {
			return err
		}
	}
	if err := writeStatusEndedBy(a.stdout, output); err != nil {
		return err
	}
	if output.EndReason != "" {
		if _, err := fmt.Fprintf(a.stdout, "  end why  %s\n", terminalSafe(output.EndReason)); err != nil {
			return err
		}
	}
	if output.EndOperationID != "" {
		if _, err := fmt.Fprintf(a.stdout, "  end batch %s\n", terminalSafe(output.EndOperationID)); err != nil {
			return err
		}
	}
	if output.RunnerVersion != "" {
		if _, err := fmt.Fprintf(a.stdout, "  runner   %s (protocol %d)\n", output.RunnerVersion, output.RunnerProtocol); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintf(a.stdout, "  runner   protocol %d\n", output.RunnerProtocol); err != nil {
		return err
	}
	if output.IdleReason != "" {
		if _, err := fmt.Fprintf(a.stdout, "  reason   %s\n", output.IdleReason); err != nil {
			return err
		}
	}
	if output.Permissions != "" {
		if _, err := fmt.Fprintf(a.stdout, "  access   %s\n", output.Permissions); err != nil {
			return err
		}
	}
	if output.Lifecycle != "" {
		if _, err := fmt.Fprintf(a.stdout, "  lifecycle %s\n", output.Lifecycle); err != nil {
			return err
		}
	}
	if output.IdleDetail != "" {
		if _, err := fmt.Fprintf(a.stdout, "  waiting  %s\n", output.IdleDetail); err != nil {
			return err
		}
	}
	if output.LastSummary != "" {
		if _, err := fmt.Fprintf(a.stdout, "  summary  %s\n", output.LastSummary); err != nil {
			return err
		}
	}
	if output.Git == nil {
		line := "  git      -"
		if output.gitMissing {
			line = "  git      not installed"
		}
		if _, err := fmt.Fprintln(a.stdout, line); err != nil {
			return err
		}
	} else {
		head := output.Git.Head
		if len(head) > 12 {
			head = head[:12]
		}
		if _, err := fmt.Fprintf(a.stdout, "  git      %s @ %s (%s dirty)\n",
			output.Git.Branch, head, strconv.Itoa(output.Git.DirtyCount)); err != nil {
			return err
		}
	}
	if output.LastVerdict != nil {
		if _, err := fmt.Fprintf(a.stdout, "  verdict  %s #%d (%d findings)\n",
			output.LastVerdict.Verdict, output.LastVerdict.Seq, output.LastVerdict.FindingCount); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(a.stdout, "  activity %s ago\n  age      %s\n", a.ageOf(lastActivityAt), formatAgeMS(output.AgeMS))
	return err
}

// writeStatusStateLines states what `sessions ls --json` reports about a
// session's state, in the same words and one fact per line. An agent reading a
// card before it sends must not have to infer working from a state word, or a
// provider failure from the absence of one.
func writeStatusStateLines(writer io.Writer, current session) error {
	if _, err := fmt.Fprintf(writer, "  working  %s\n  exited   %s\n",
		yesOrNo(current.Working), yesOrNo(current.Exited)); err != nil {
		return err
	}
	if current.FailureKind == "" {
		return nil
	}
	failure := current.FailureKind
	if current.FailureDetail != "" {
		failure += " — " + terminalSafe(current.FailureDetail)
	}
	_, err := fmt.Fprintf(writer, "  failure  %s\n", failure)
	return err
}

func writeStatusEndedBy(writer io.Writer, output statusOutput) error {
	if output.EndedByKind == "" && output.EndedByClient == "" {
		return nil
	}
	endedBy := output.EndedByKind
	if output.EndedByName != "" {
		endedBy = terminalSafe(output.EndedByName)
	}
	if output.EndedByID != "" && output.EndedByName == "" {
		if endedBy != "" {
			endedBy += ":"
		}
		endedBy += terminalSafe(output.EndedByID)
	}
	if endedBy == "" {
		endedBy = terminalSafe(output.EndedByClient)
	} else if output.EndedByClient != "" {
		endedBy += " via " + terminalSafe(output.EndedByClient)
	}
	_, err := fmt.Fprintf(writer, "  ended by %s\n", endedBy)
	return err
}

func yesOrNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func terminalSafe(value string) string {
	return strings.Map(func(char rune) rune {
		if unicode.IsControl(char) {
			return -1
		}
		return char
	}, value)
}

func formatAgeMS(milliseconds int64) string {
	if milliseconds < time.Minute.Milliseconds() {
		return fmt.Sprintf("%ds", milliseconds/1000)
	}
	if milliseconds < time.Hour.Milliseconds() {
		return fmt.Sprintf("%dm", milliseconds/time.Minute.Milliseconds())
	}
	return fmt.Sprintf("%dh", milliseconds/time.Hour.Milliseconds())
}
