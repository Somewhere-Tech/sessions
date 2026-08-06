package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

var keyBytes = map[string]string{
	"esc": "\x1b", "escape": "\x1b", "up": "\x1b[A", "down": "\x1b[B",
	"left": "\x1b[D", "right": "\x1b[C", "^c": "\x03", "ctrlc": "\x03",
	"^d": "\x04", "ctrld": "\x04", "enter": "\r", "tab": "\t",
}

var keyOrder = []string{"esc", "escape", "up", "down", "left", "right", "^c", "ctrlc", "^d", "ctrld", "enter", "tab"}

func (a *app) cmdKeys(args []string) error {
	if len(args) < 2 || args[0] == "" || args[1] == "" {
		return fail(1, "usage: sessions keys <id> <esc|up|down|left|right|^c|^d|enter|tab>")
	}
	data, ok := keyBytes[strings.ToLower(args[1])]
	if !ok {
		return fail(1, "unknown key '%s'. valid: %s", args[1], strings.Join(keyOrder, ", "))
	}
	id, err := a.resolveSessionID(args[0])
	if err != nil {
		return err
	}
	return a.postJSON("/api/sessions/"+escapeID(id)+"/input", map[string]string{"data": data}, &map[string]any{}, 2)
}

type toolPreset struct {
	command  string
	args     []string
	fullArgs []string
}

var toolPresets = map[string]toolPreset{
	"claude": {
		command: "claude", args: []string{}, fullArgs: []string{"--dangerously-skip-permissions"},
	},
	"codex": {
		command:  "codex",
		args:     []string{"-c", "check_for_update_on_startup=false", "--sandbox", "workspace-write", "--ask-for-approval", "on-request"},
		fullArgs: []string{"-c", "check_for_update_on_startup=false", "--dangerously-bypass-approvals-and-sandbox"},
	},
	"shell": {},
}

var toolPresetOrder = []string{"claude", "codex", "shell"}

type createSessionRequest struct {
	Cmd         string            `json:"cmd,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Cwd         string            `json:"cwd,omitempty"`
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
	Profile     string            `json:"profile,omitempty"`
	Worktree    bool              `json:"worktree,omitempty"`
	Base        string            `json:"base,omitempty"`
	OnIdle      string            `json:"onIdle,omitempty"`
	WaitReady   bool              `json:"waitReady,omitempty"`
	Kind        string            `json:"kind,omitempty"`
	Force       bool              `json:"force,omitempty"`
	Permissions string            `json:"permissions,omitempty"`
	Lifecycle   string            `json:"lifecycle,omitempty"`
}

type agentControls struct {
	model  *string
	effort *string
	fast   bool
}

func applyToolDefault(body *createSessionRequest, fullAccess bool) error {
	if body.Cmd == "" {
		return nil
	}
	base := strings.ToLower(filepath.Base(body.Cmd))
	preset, ok := toolPresets[base]
	if !ok || preset.args == nil {
		return nil
	}
	for _, argument := range body.Args {
		switch argument {
		case "--dangerously-bypass-approvals-and-sandbox", "--dangerously-skip-permissions", "--sandbox", "--ask-for-approval", "--full-auto":
			return nil
		}
	}
	defaults := preset.args
	if fullAccess {
		defaults = preset.fullArgs
	}
	body.Args = append(append([]string(nil), defaults...), body.Args...)
	if base == "claude" && !hasAnyArg(body.Args, "--session-id", "--resume") {
		id, err := randomUUID()
		if err != nil {
			return err
		}
		body.Args = append(body.Args, "--session-id", id)
	}
	return nil
}

func hasAnyArg(args []string, values ...string) bool {
	for _, arg := range args {
		for _, value := range values {
			if arg == value {
				return true
			}
		}
	}
	return false
}

func hasArgValue(args []string, values ...string) bool {
	for index, arg := range args {
		for _, value := range values {
			if arg == value && index+1 < len(args) {
				return true
			}
		}
	}
	return false
}

func hasConfigValue(args []string, key string) bool {
	for index, arg := range args {
		if (arg == "-c" || arg == "--config") && index+1 < len(args) && strings.HasPrefix(args[index+1], key+"=") {
			return true
		}
	}
	return false
}

func applyAgentControls(body *createSessionRequest, controls agentControls) error {
	if controls.model == nil && controls.effort == nil && !controls.fast {
		return nil
	}
	base := "shell"
	if body.Cmd != "" {
		base = strings.ToLower(filepath.Base(body.Cmd))
	}
	if base != "claude" && base != "codex" {
		return fail(1, "--model, --effort, and --fast are only for claude/codex")
	}
	if base == "claude" && controls.fast {
		return fail(1, "--fast is not supported for claude (claude has no service tier)")
	}
	if controls.model != nil && !hasArgValue(body.Args, "--model", "-m") {
		body.Args = append(body.Args, "--model", *controls.model)
	}
	if controls.effort != nil {
		if base == "claude" && !hasArgValue(body.Args, "--effort") {
			body.Args = append(body.Args, "--effort", *controls.effort)
		} else if base == "codex" && !hasConfigValue(body.Args, "model_reasoning_effort") {
			body.Args = append(body.Args, "-c", fmt.Sprintf("model_reasoning_effort=\"%s\"", *controls.effort))
		}
	}
	if controls.fast && !hasConfigValue(body.Args, "service_tier") {
		body.Args = append(body.Args, "-c", "service_tier=\"priority\"")
	}
	return nil
}

func pluckControl(args *[]string, name string) (*string, error) {
	for index, arg := range *args {
		if arg != name {
			continue
		}
		if index+1 >= len(*args) || strings.HasPrefix((*args)[index+1], "--") {
			return nil, fail(1, "%s needs a value", name)
		}
		value := (*args)[index+1]
		*args = append((*args)[:index], (*args)[index+2:]...)
		return &value, nil
	}
	return nil, nil
}

func pluckDescription(args *[]string) (string, error) {
	description, full := pluck(args, "--description")
	alias, short := pluck(args, "--desc")
	if full && short {
		return "", fail(1, "--description and --desc cannot be combined")
	}
	if short {
		description = alias
	}
	if (full || short) && strings.TrimSpace(description) == "" {
		return "", fail(1, "--description needs a non-empty purpose")
	}
	return strings.TrimSpace(description), nil
}

func pluckTags(args *[]string) (map[string]string, error) {
	tags := make(map[string]string)
	for index := 0; index < len(*args); {
		if (*args)[index] != "--tag" {
			index++
			continue
		}
		if index+1 >= len(*args) || strings.HasPrefix((*args)[index+1], "--") {
			return nil, fail(1, "--tag needs key=value (for example --tag product=Sessions)")
		}
		raw := (*args)[index+1]
		key, value, found := strings.Cut(raw, "=")
		if !found {
			return nil, fail(1, "invalid tag %q; use key=value (for example --tag product=Sessions)", raw)
		}
		key = strings.TrimSpace(key)
		if _, duplicate := tags[strings.ToLower(key)]; duplicate {
			return nil, fail(1, "tag key %q was supplied more than once", strings.ToLower(key))
		}
		tags[key] = value
		*args = append((*args)[:index], (*args)[index+2:]...)
	}
	normalized, err := state.NormalizeTags(tags)
	if err != nil {
		return nil, fail(1, "%s", err)
	}
	return normalized, nil
}

func pluckWorktreeOptions(args *[]string) (bool, string, error) {
	worktree := removeFirst(args, "--worktree")
	base, hasBase := pluck(args, "--base")
	base = strings.TrimSpace(base)
	if hasBase && (base == "" || strings.HasPrefix(base, "-")) {
		return false, "", fail(1, "--base needs a branch or ref")
	}
	if hasBase && !worktree {
		return false, "", fail(1, "--base requires --worktree")
	}
	return worktree, base, nil
}

func (a *app) cmdNew(args []string) error {
	if err := a.configureCreateOwner(&args); err != nil {
		return err
	}
	var body createSessionRequest
	description, err := pluckDescription(&args)
	if err != nil {
		return err
	}
	body.Description = description
	body.Tags, err = pluckTags(&args)
	if err != nil {
		return err
	}
	if value, present := pluck(&args, "--profile"); present {
		if strings.HasPrefix(value, "-") || value == "" {
			return fail(1, "--profile needs a name")
		}
		if err := state.ValidateProfileName(value); err != nil {
			return fail(1, "%s", err)
		}
		body.Profile = value
	}
	body.Worktree, body.Base, err = pluckWorktreeOptions(&args)
	if err != nil {
		return err
	}
	body.Force = removeFirst(&args, "--force")
	forceStructuredClaude := removeFirst(&args, "--structured")
	forcePTYClaude := removeFirst(&args, "--pty-claude")
	forceAppServer := removeFirst(&args, "--codex-appserver")
	forcePTYCodex := removeFirst(&args, "--pty-codex")
	if forceStructuredClaude && forcePTYClaude {
		return fail(1, "--structured and --pty-claude cannot be combined")
	}
	if forceAppServer && forcePTYCodex {
		return fail(1, "--codex-appserver and --pty-codex cannot be combined")
	}
	model, err := pluckControl(&args, "--model")
	if err != nil {
		return err
	}
	effort, err := pluckControl(&args, "--effort")
	if err != nil {
		return err
	}
	fast := removeFirst(&args, "--fast")
	if value, present := pluck(&args, "--cwd"); present {
		body.Cwd = value
	}
	if value, present := pluck(&args, "--name"); present {
		if strings.TrimSpace(value) == "" {
			return fail(1, "--name needs a non-empty label")
		}
		body.Name = strings.TrimSpace(value)
	}
	if value, present := pluck(&args, "--on-idle"); present {
		if strings.TrimSpace(value) == "" {
			return fail(1, "--on-idle needs a shell command")
		}
		body.OnIdle = value
	}
	body.WaitReady = removeFirst(&args, "--wait-ready")
	tool, hasTool := pluck(&args, "--tool")
	initialInput := ""
	fullAccess := removeFirst(&args, "--full-access")
	if value, present := pluck(&args, "--permissions"); present {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != state.PermissionsInherit && value != state.PermissionsConstrained && value != state.PermissionsFull {
			return fail(1, "--permissions must be inherit, constrained, or full")
		}
		body.Permissions = value
	}
	if fullAccess {
		if body.Permissions != "" && body.Permissions != state.PermissionsFull {
			return fail(1, "--full-access conflicts with --permissions %s", body.Permissions)
		}
		body.Permissions = state.PermissionsFull
	}
	fullAccess = body.Permissions == state.PermissionsFull
	if value, present := pluck(&args, "--lifecycle"); present {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != state.LifecycleTask && value != state.LifecycleSession {
			return fail(1, "--lifecycle must be task or session")
		}
		body.Lifecycle = value
	}
	if removeFirst(&args, "--keep-alive") {
		if body.Lifecycle == state.LifecycleTask {
			return fail(1, "--keep-alive conflicts with --lifecycle task")
		}
		body.Lifecycle = state.LifecycleSession
	}
	// Compatibility for scripts written before constrained execution became
	// the public default. It is now an explicit no-op, not a mode switch.
	noSkipPermissions := removeFirst(&args, "--no-skip-perms")
	if fullAccess && noSkipPermissions {
		return fail(1, "--full-access and --no-skip-perms cannot be combined")
	}
	if hasTool {
		preset, ok := toolPresets[strings.ToLower(tool)]
		if !ok {
			return fail(1, "unknown --tool '%s'. valid: %s", tool, strings.Join(toolPresetOrder, ", "))
		}
		body.Cmd = preset.command
		chosen := preset.args
		if fullAccess {
			chosen = preset.fullArgs
		}
		if chosen != nil {
			body.Args = append([]string(nil), chosen...)
		}
		body.Args = append(body.Args, args...)
		if strings.EqualFold(tool, "codex") {
			if forceStructuredClaude || forcePTYClaude {
				return fail(1, "--structured and --pty-claude are only valid with --tool claude")
			}
			if forceAppServer && !fullAccess {
				return fail(1, "--codex-appserver currently requires --full-access because Sessions cannot yet present app-server approval prompts; use sandboxed --pty-codex otherwise")
			}
			if fullAccess && !forcePTYCodex && (forceAppServer || codexAppServerEnabled()) {
				body.Kind = "codex-app-server"
				// The app-server runtime does not consume positional CLI arguments.
				// Treat them as the first user request and deliver them through the
				// same audited input route used by `sessions send` and the desktop UI.
				// This is an immediate post-create send, never a hidden prompt queue.
				if len(args) > 0 {
					initialInput = strings.Join(args, " ")
					body.Args = append([]string(nil), chosen...)
				}
			}
		} else if forceAppServer || forcePTYCodex {
			return fail(1, "--codex-appserver and --pty-codex are only valid with --tool codex")
		}
		if strings.EqualFold(tool, "claude") {
			if forceStructuredClaude {
				body.Kind = "claude-structured"
			}
		} else if forceStructuredClaude || forcePTYClaude {
			return fail(1, "--structured and --pty-claude are only valid with --tool claude")
		}
		if strings.EqualFold(tool, "claude") && !hasAnyArg(body.Args, "--session-id", "--resume", "-r") {
			id, err := randomUUID()
			if err != nil {
				return err
			}
			body.Args = append(body.Args, "--session-id", id)
		}
	} else {
		if forceAppServer || forcePTYCodex {
			return fail(1, "--codex-appserver and --pty-codex require --tool codex")
		}
		if command, present := pluck(&args, "--cmd"); present {
			body.Cmd = command
			body.Args = append([]string(nil), args...)
		} else if len(args) > 0 {
			body.Cmd = args[0]
			body.Args = append([]string(nil), args[1:]...)
		}
		if err := applyToolDefault(&body, fullAccess); err != nil {
			return err
		}
		if state.CommandTool(body.Cmd) == state.ToolClaude {
			if forceStructuredClaude {
				body.Kind = state.KindClaudeStructured
			}
		} else if forceStructuredClaude || forcePTYClaude {
			return fail(1, "--structured and --pty-claude require a Claude command")
		}
	}
	if err := applyAgentControls(&body, agentControls{model: model, effort: effort, fast: fast}); err != nil {
		return err
	}
	if body.Profile != "" {
		tool := state.CommandTool(body.Cmd)
		if _, supported := state.ProfileToolName(tool); !supported {
			return fail(1, "--profile is only for Claude or Codex sessions; remove it for shell sessions")
		}
	}
	if body.Worktree {
		if body.Cwd == "" {
			body.Cwd, err = os.Getwd()
		} else {
			body.Cwd, err = filepath.Abs(body.Cwd)
		}
		if err != nil {
			return fail(1, "resolve worktree source cwd: %s", err)
		}
	}
	var info map[string]any
	if err := a.postJSON("/api/sessions", body, &info, 2); err != nil {
		return err
	}
	if strings.TrimSpace(initialInput) != "" {
		id := strings.TrimSpace(fmt.Sprint(info["id"]))
		if id == "" {
			return fail(2, "session was created, but sessionsd did not return its id; first request was not sent")
		}
		result, err := a.sendAndConfirm(id, initialInput, 30*time.Second, false)
		if err != nil {
			return fail(2, "session %s was created, but its first request was not sent: %s", id, err)
		}
		if result.ExitCode != 0 {
			return fail(2, "session %s was created, but its first request was not confirmed: %s", id, result.Reason)
		}
	}
	if a.wantJSON {
		return writeJSON(a.stdout, info, true)
	}
	_, err = fmt.Fprintln(a.stdout, info["id"])
	return err
}

func codexAppServerEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SESSIONS_CODEX_APPSERVER"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func (a *app) cmdModel(args []string) error {
	if len(args) < 2 || args[0] == "" || args[1] == "" {
		return fail(1, "usage: sessions model <session> <model> [--effort <level>]")
	}
	idArg, model := args[0], args[1]
	args = args[2:]
	effort, present := pluck(&args, "--effort")
	if present && effort == "" {
		return fail(1, "--effort needs a value")
	}
	if len(args) > 0 {
		return fail(1, "usage: sessions model <session> <model> [--effort <level>]")
	}
	id, err := a.resolveSessionID(idArg)
	if err != nil {
		return err
	}
	body := map[string]any{"model": model}
	if present {
		body["effort"] = effort
	}
	var updated session
	if err := a.putJSON("/api/sessions/"+escapeID(id)+"/model", body, &updated, 2); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, updated, true)
	}
	if updated.Effort != "" {
		_, err = fmt.Fprintf(a.stdout, "Next turn: %s · %s\n", updated.Model, updated.Effort)
	} else {
		_, err = fmt.Fprintf(a.stdout, "Next turn: %s\n", updated.Model)
	}
	return err
}

const (
	killStatusKilled        = "killed"
	killStatusAlreadyExited = "already-exited"
	killStatusFailed        = "failed"
	killStatusUnconfirmed   = "unconfirmed"
)

// killItem and killResult mirror the per-target result shape already used by
// archive and aside so agents parse one termination contract, not three.
type killItem struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type killResult struct {
	Items       []killItem `json:"items"`
	OperationID string     `json:"operation_id,omitempty"`
}

// killBatchResponse is the /api/sessions/end-batch success contract: the
// daemon confirms the batch with ok and echoes the ids it actually ended.
// Anything it does not confirm is reported as such instead of assumed dead.
type killBatchResponse struct {
	OK    *bool    `json:"ok"`
	IDs   []string `json:"ids"`
	Error string   `json:"error,omitempty"`
}

func (r killBatchResponse) classify(targets []string) []killItem {
	items := make([]killItem, 0, len(targets))
	switch {
	case r.OK != nil && !*r.OK:
		reason := strings.TrimSpace(r.Error)
		if reason == "" {
			reason = "the daemon reported the batch termination as unsuccessful"
		}
		for _, id := range targets {
			items = append(items, killItem{ID: id, Status: killStatusFailed, Reason: reason})
		}
	case len(r.IDs) > 0:
		confirmed := make(map[string]struct{}, len(r.IDs))
		for _, id := range r.IDs {
			confirmed[id] = struct{}{}
		}
		for _, id := range targets {
			if _, ok := confirmed[id]; ok {
				items = append(items, killItem{ID: id, Status: killStatusKilled})
				continue
			}
			items = append(items, killItem{
				ID: id, Status: killStatusFailed,
				Reason: "the daemon did not report this session as ended",
			})
		}
	case r.OK != nil && *r.OK:
		for _, id := range targets {
			items = append(items, killItem{ID: id, Status: killStatusKilled})
		}
	default:
		for _, id := range targets {
			items = append(items, killItem{
				ID: id, Status: killStatusUnconfirmed,
				Reason: "the daemon accepted the batch without confirming which sessions ended",
			})
		}
	}
	return items
}

// reportKill prints per-target truth and fails when a requested termination
// was refused (exit 1) or could not be confirmed at all (exit 2).
func (a *app) reportKill(result killResult) error {
	failed := make([]killItem, 0, len(result.Items))
	unconfirmed := make([]killItem, 0, len(result.Items))
	for _, item := range result.Items {
		switch item.Status {
		case killStatusFailed:
			failed = append(failed, item)
		case killStatusUnconfirmed:
			unconfirmed = append(unconfirmed, item)
		}
	}
	// One bad target explains itself in the failure message; a mixed batch needs
	// a line per target so the caller can tell which ids still need attention.
	detailed := len(failed)+len(unconfirmed) > 1
	if a.wantJSON {
		if err := writeJSON(a.stdout, result, true); err != nil {
			return err
		}
	} else {
		for _, item := range result.Items {
			switch item.Status {
			case killStatusKilled:
				fmt.Fprintf(a.stdout, "killed %s\n", item.ID)
			case killStatusAlreadyExited:
				fmt.Fprintf(a.stdout, "lane %s already exited; nothing to kill\n", item.ID)
			default:
				if detailed {
					fmt.Fprintf(a.stderr, "%s %s: %s\n", item.Status, item.ID, item.Reason)
				}
			}
		}
	}
	if len(failed) == 1 && len(unconfirmed) == 0 {
		return fail(1, "kill did not end %s: %s — run `sessions ls`, then retry `sessions kill %s`",
			failed[0].ID, failed[0].Reason, failed[0].ID)
	}
	if len(unconfirmed) == 1 && len(failed) == 0 {
		return fail(2, "kill could not confirm %s: %s — run `sessions ls` to see whether it ended before retrying",
			unconfirmed[0].ID, unconfirmed[0].Reason)
	}
	if len(failed) > 0 {
		return fail(1, "kill did not end %d of %d target(s): %s — run `sessions status <id>` on each one, then retry `sessions kill <id>`",
			len(failed), len(result.Items), strings.Join(killItemIDs(failed), " "))
	}
	if len(unconfirmed) > 0 {
		return fail(2, "kill could not confirm %d of %d target(s): %s — run `sessions ls` to see whether they ended before retrying",
			len(unconfirmed), len(result.Items), strings.Join(killItemIDs(unconfirmed), " "))
	}
	return nil
}

func killItemIDs(items []killItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func (a *app) cmdKill(ids []string) error {
	reason, hasReason := pluck(&ids, "--reason")
	force := removeFirst(&ids, "--force")
	if hasReason {
		reason = strings.TrimSpace(reason)
		if reason == "" {
			return fail(1, "--reason needs a non-empty explanation")
		}
	}
	if len(ids) == 0 {
		return fail(1, "usage: sessions kill <id> [<id>...] [--reason <text>] [--force]")
	}
	for _, value := range ids {
		if strings.HasPrefix(value, "-") {
			return fail(1, "unknown kill option %s", value)
		}
	}
	operationID := ""
	if len(ids) > 1 {
		var err error
		operationID, err = randomUUID()
		if err != nil {
			return fail(2, "create batch operation id: %s", err)
		}
	}
	result := killResult{Items: make([]killItem, 0, len(ids)), OperationID: operationID}
	resolved := make([]string, 0, len(ids))
	for _, idArg := range ids {
		laneID, isLane, err := a.resolveLaneID(idArg)
		if err != nil {
			return err
		}
		if isLane {
			_, statusCode, err := a.fetchLaneManifest(context.Background(), laneID)
			if err != nil {
				return err
			}
			if statusCode == http.StatusOK {
				result.Items = append(result.Items, killItem{ID: laneID, Status: killStatusAlreadyExited})
				continue
			}
		}
		id := laneID
		if !isLane {
			id, err = a.resolveSessionID(idArg)
			if err != nil {
				return err
			}
		}
		listed, err := a.listSessions(true)
		if err != nil {
			return err
		}
		alreadyExitedLane := false
		for _, candidate := range listed {
			if candidate.ID == id && candidate.Kind == "lane" && candidate.Exited {
				alreadyExitedLane = true
				break
			}
		}
		if alreadyExitedLane {
			result.Items = append(result.Items, killItem{ID: id, Status: killStatusAlreadyExited})
			continue
		}
		if !slices.Contains(resolved, id) {
			resolved = append(resolved, id)
		}
	}
	if len(resolved) == 0 {
		return a.reportKill(result)
	}
	if len(resolved) > 1 {
		var response killBatchResponse
		err := a.postJSON("/api/sessions/end-batch", map[string]any{
			"ids": resolved, "reason": reason, "operationId": operationID, "force": force,
		}, &response, 2)
		if err != nil {
			return err
		}
		result.Items = append(result.Items, response.classify(resolved)...)
		return a.reportKill(result)
	}
	path := "/api/sessions/" + escapeID(resolved[0])
	if force {
		path += "?force=1"
	}
	ok, err := a.delete(path, map[string]string{
		"reason": reason, "operationId": operationID,
	})
	if err != nil {
		return err
	}
	if !ok {
		result.Items = append(result.Items, killItem{
			ID: resolved[0], Status: killStatusFailed, Reason: unknownSessionMessage(resolved[0]),
		})
		return a.reportKill(result)
	}
	result.Items = append(result.Items, killItem{ID: resolved[0], Status: killStatusKilled})
	return a.reportKill(result)
}

// waitOutcome is the single shape every `sessions wait <session>` branch
// returns. It used to be four different objects: the target's identity was
// suppressed unless --summary, idleMs was missing from two of them, and `ok`
// meant "the call worked" in one branch and "the condition was met" in
// another. A delegating agent could not write one parser, and the branch that
// mattered most — the target is gone — reported success.
//
// ok answers one question only: can the caller stop waiting and act? Idle and
// needs-input are both actionable. Gone, failed, and timeout are not.
type waitOutcome struct {
	OK         bool   `json:"ok"`
	Reason     string `json:"reason"`
	Session    string `json:"session"`
	Working    bool   `json:"working"`
	IdleMS     int64  `json:"idleMs"`
	IdleReason string `json:"idleReason,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

const (
	waitReasonIdle       = "idle"
	waitReasonNeedsInput = "needs-input"
	waitReasonFailed     = "failed"
	waitReasonGone       = "gone"
	waitReasonTimeout    = "timeout"
)

// writeWaitOutcome emits the envelope and returns the exit status that matches
// it, so the JSON and the exit code can never disagree.
func (a *app) writeWaitOutcome(outcome waitOutcome, humanText string, humanToStderr bool) error {
	if a.wantJSON {
		if err := writeJSON(a.stdout, outcome, false); err != nil {
			return err
		}
	} else {
		destination := a.stdout
		if humanToStderr {
			destination = a.stderr
		}
		if _, err := io.WriteString(destination, humanText+"\n"); err != nil {
			return err
		}
	}
	switch outcome.Reason {
	case waitReasonTimeout:
		return status(exitWaitTimeout)
	case waitReasonGone, waitReasonFailed:
		return status(exitTargetUnavailable)
	default:
		return nil
	}
}

func (a *app) cmdWait(args []string) error {
	if isWaitUntilArgs(args) {
		return a.cmdWaitUntil(args)
	}
	if len(args) == 0 || args[0] == "" {
		return fail(1, "usage: sessions wait <id> [--idle 2s] [--timeout 30s]")
	}
	idArg := args[0]
	args = args[1:]
	includeSummary := removeFirst(&args, "--summary")
	idle := 2 * time.Second
	timeout := 30 * time.Second
	var err error
	if raw, present := pluck(&args, "--idle"); present && raw != "" {
		idle, err = parseDuration(raw, 0)
		if err != nil {
			return err
		}
	}
	if raw, present := pluck(&args, "--timeout"); present && raw != "" {
		timeout, err = parseDuration(raw, 0)
		if err != nil {
			return err
		}
	}
	if len(args) != 0 {
		return fail(1, "usage: sessions wait <id> [--idle 2s] [--timeout 30s] [--summary]")
	}
	id, err := a.resolveSessionID(idArg)
	if err != nil {
		return err
	}
	start := a.now()
	poll := idle / 4
	if poll < 100*time.Millisecond {
		poll = 100 * time.Millisecond
	}
	if poll > 500*time.Millisecond {
		poll = 500 * time.Millisecond
	}
	var notWorkingSince time.Time
	for {
		sessions, err := a.listSessions(false)
		if err != nil {
			return err
		}
		var current *session
		for index := range sessions {
			if sessions[index].ID == id {
				current = &sessions[index]
				break
			}
		}
		if current == nil {
			// A target that vanished is the outcome a delegating agent most
			// needs to distinguish, and it used to report ok:true and exit 0 —
			// so every loop written as `if rc == 0` treated a dead delegate as
			// a finished one.
			return a.writeWaitOutcome(waitOutcome{
				OK:      false,
				Reason:  waitReasonGone,
				Session: id,
			}, "gone", false)
		}
		if !current.Working && (current.IdleReason == state.IdleReasonNeedsInput || current.IdleReason == state.IdleReasonFailed) {
			reason := waitReasonNeedsInput
			if current.IdleReason == state.IdleReasonFailed {
				reason = waitReasonFailed
			}
			message := current.LastSummary
			if current.IdleReason == state.IdleReasonNeedsInput && current.IdleDetail != "" {
				message = current.IdleDetail
			}
			if message == "" {
				message = current.IdleReason
			}
			return a.writeWaitOutcome(waitOutcome{
				OK:         reason == waitReasonNeedsInput,
				Reason:     reason,
				Session:    current.ID,
				Working:    false,
				IdleReason: current.IdleReason,
				Detail:     current.IdleDetail,
				Summary:    current.LastSummary,
			}, fmt.Sprintf("%s — %s", current.IdleReason, message), reason == waitReasonFailed)
		}
		idleFor := time.Duration(0)
		if isConfirmableTool(toolOfSession(*current)) {
			if current.Working {
				notWorkingSince = time.Time{}
			} else if notWorkingSince.IsZero() {
				notWorkingSince = a.now()
			}
			if !notWorkingSince.IsZero() {
				idleFor = a.now().Sub(notWorkingSince)
			}
		} else {
			base := current.LastDataAt
			if base == 0 {
				base = current.CreatedAt
			}
			idleFor = a.now().Sub(time.UnixMilli(base))
		}
		if idleFor >= idle {
			summary := current.LastSummary
			if summary == "" {
				summary = current.IdleDetail
			}
			if summary == "" {
				summary = current.IdleReason
			}
			humanText := fmt.Sprintf("idle for %dms", idleFor.Milliseconds())
			if includeSummary {
				humanText = fmt.Sprintf("%s — %s", current.ID, summary)
			}
			// --summary now only decides how much prose comes back. It used to
			// decide whether the caller learned which session answered, which
			// made the schema depend on a display flag.
			outcome := waitOutcome{
				OK:         true,
				Reason:     waitReasonIdle,
				Session:    current.ID,
				Working:    current.Working,
				IdleMS:     idleFor.Milliseconds(),
				IdleReason: current.IdleReason,
			}
			if includeSummary {
				outcome.Summary = current.LastSummary
			}
			return a.writeWaitOutcome(outcome, humanText, false)
		}
		if a.now().Sub(start) >= timeout {
			return a.writeWaitOutcome(waitOutcome{
				OK:      false,
				Reason:  waitReasonTimeout,
				Session: current.ID,
				Working: current.Working,
				IdleMS:  idleFor.Milliseconds(),
			}, fmt.Sprintf("timeout: still active after %dms (last %dms ago)",
				timeout.Milliseconds(), idleFor.Milliseconds()), true)
		}
		a.sleep(poll)
	}
}

func positiveInt(raw, label string) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fail(1, "%s must be a positive integer", label)
	}
	return value, nil
}

func executableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}
