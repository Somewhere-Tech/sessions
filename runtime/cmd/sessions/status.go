package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	ID                string                   `json:"id"`
	Name              string                   `json:"name"`
	Description       string                   `json:"description"`
	DescriptionSource string                   `json:"description_source,omitempty"`
	Kind              string                   `json:"kind"`
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
}

func (a *app) cmdStatus(args []string) error {
	if len(args) != 1 || args[0] == "" {
		return fail(1, "usage: sessions status <id> [--json]")
	}
	current, err := a.resolveStatusSession(args[0])
	if err != nil {
		return err
	}
	id := current.ID

	git, err := inspectGit(current.Cwd)
	if err != nil {
		return fail(2, "inspect git in %s: %s", current.Cwd, err)
	}
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
		DescriptionSource: current.DescriptionSource, Kind: "session", Tool: toolOfSession(*current),
		State: state, Cwd: current.Cwd, Profile: current.Profile, ConfigDir: current.ConfigDir,
		WorktreePath: current.WorktreePath, Branch: current.Branch, Base: current.Base, SourceRepo: current.SourceRepo,
		Git: git, LastVerdict: summary,
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
		UnreachableSince: current.UnreachableSince,
	}
	if current.Exited {
		output.ExitCode = current.ExitCode
	}
	if a.wantJSON {
		return writeJSON(a.stdout, output, true)
	}
	return a.writeStatusCard(output, lastActivityAt)
}

// liveStatusState describes the runtime that exists now. IdleReason describes
// the last turn. Keeping those axes separate prevents a completed turn from
// making a reusable, live session look terminal to a person or a watcher.
func liveStatusState(current session) string {
	if current.Exited {
		return "exited"
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

func (a *app) resolveStatusSession(idOrPrefix string) (*session, error) {
	sessions, err := a.listSessions(true)
	if err != nil {
		return nil, err
	}
	for index := range sessions {
		if sessions[index].ID == idOrPrefix {
			return &sessions[index], nil
		}
	}
	matches := make([]int, 0)
	for index := range sessions {
		if strings.HasPrefix(sessions[index].ID, idOrPrefix) {
			matches = append(matches, index)
		}
	}
	if len(matches) == 1 {
		return &sessions[matches[0]], nil
	}
	if len(matches) == 0 {
		return nil, fail(1, "%s", unknownSessionMessage(idOrPrefix))
	}
	var lines strings.Builder
	for _, index := range matches {
		candidate := sessions[index]
		fmt.Fprintf(&lines, "  %s  %s\n", prefixString(candidate.ID, 8), a.sessionLabel(candidate))
	}
	return nil, fail(1, "ambiguous session prefix '%s' — matches:\n%srun `sessions ls`", idOrPrefix, lines.String())
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

func inspectGit(cwd string) (*gitStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	probe := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--is-inside-work-tree")
	probeOutput, err := probe.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, nil
		}
		return nil, err
	}
	if strings.TrimSpace(string(probeOutput)) != "true" {
		return nil, nil
	}
	rootOutput, err := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil, err
	}
	root := strings.TrimSpace(string(rootOutput))
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		absHome, homeAbsErr := filepath.Abs(home)
		absCWD, cwdAbsErr := filepath.Abs(cwd)
		absRoot, rootAbsErr := filepath.Abs(root)
		if homeAbsErr == nil && cwdAbsErr == nil && rootAbsErr == nil && absHome == absRoot && absCWD == absRoot {
			return nil, nil
		}
	}
	command := exec.CommandContext(ctx, "git", "-C", cwd, "status", "--porcelain=v2", "--branch", "--untracked-files=normal", "--", ".")
	encoded, err := command.Output()
	if err != nil {
		return nil, err
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
	return result, nil
}

func formatStatusTime(milliseconds int64) string {
	return time.UnixMilli(milliseconds).UTC().Format(time.RFC3339Nano)
}

func (a *app) writeStatusCard(output statusOutput, lastActivityAt int64) error {
	label := output.Name
	if label == "" {
		label = prefixString(output.ID, 8)
	}
	if _, err := fmt.Fprintf(a.stdout, "%s  %s\n", label, output.State); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(a.stdout, "  id       %s\n  kind     %s\n  tool     %s\n  cwd      %s\n",
		output.ID, output.Kind, output.Tool, a.homeRelative(output.Cwd)); err != nil {
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
		detail := output.UnreachableReason
		if detail == "restart-restore-pending" {
			detail = "paused after reboot — run `sessions resume " + output.ID + "`"
		}
		if _, err := fmt.Fprintf(a.stdout, "  recovery %s\n", detail); err != nil {
			return err
		}
	}
	if output.EndedByKind != "" || output.EndedByClient != "" {
		endedBy := output.EndedByKind
		if output.EndedByName != "" {
			endedBy = terminalSafe(output.EndedByName)
		}
		if output.EndedByID != "" {
			if endedBy != "" && output.EndedByName == "" {
				endedBy += ":"
			}
			if output.EndedByName == "" {
				endedBy += terminalSafe(output.EndedByID)
			}
		}
		if endedBy == "" {
			endedBy = terminalSafe(output.EndedByClient)
		} else if output.EndedByClient != "" {
			endedBy += " via " + terminalSafe(output.EndedByClient)
		}
		if _, err := fmt.Fprintf(a.stdout, "  ended by %s\n", endedBy); err != nil {
			return err
		}
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
		if _, err := fmt.Fprintln(a.stdout, "  git      -"); err != nil {
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
