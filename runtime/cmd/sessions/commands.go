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
	fullAccess := removeFirst(&args, "--full-access")
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
				fmt.Fprintf(a.stdout, "lane %s already exited; nothing to kill\n", laneID)
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
			fmt.Fprintf(a.stdout, "lane %s already exited; nothing to kill\n", id)
			continue
		}
		if !slices.Contains(resolved, id) {
			resolved = append(resolved, id)
		}
	}
	if len(resolved) == 0 {
		return nil
	}
	if len(resolved) > 1 {
		var result map[string]any
		err := a.postJSON("/api/sessions/end-batch", map[string]any{
			"ids": resolved, "reason": reason, "operationId": operationID, "force": force,
		}, &result, 2)
		if err != nil {
			return err
		}
		for _, id := range resolved {
			fmt.Fprintf(a.stdout, "killed %s\n", id)
		}
		return nil
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
		fmt.Fprintln(a.stderr, unknownSessionMessage(resolved[0]))
		return status(1)
	}
	fmt.Fprintf(a.stdout, "killed %s\n", resolved[0])
	return nil
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
			if a.wantJSON {
				return writeJSON(a.stdout, struct {
					OK     bool   `json:"ok"`
					Reason string `json:"reason"`
				}{true, "gone"}, false)
			}
			_, err := io.WriteString(a.stdout, "gone\n")
			return err
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
			if a.wantJSON {
				return writeJSON(a.stdout, struct {
					OK         bool   `json:"ok"`
					Session    string `json:"session,omitempty"`
					Reason     string `json:"reason"`
					IdleReason string `json:"idleReason,omitempty"`
					Summary    string `json:"summary,omitempty"`
					IdleMS     int64  `json:"idleMs"`
					Working    bool   `json:"working"`
				}{
					true, func() string {
						if includeSummary {
							return current.ID
						}
						return ""
					}(), "idle", current.IdleReason,
					func() string {
						if includeSummary {
							return current.LastSummary
						}
						return ""
					}(),
					idleFor.Milliseconds(), current.Working,
				}, false)
			}
			if includeSummary {
				summary := current.LastSummary
				if summary == "" {
					summary = current.IdleDetail
				}
				if summary == "" {
					summary = current.IdleReason
				}
				_, err := fmt.Fprintf(a.stdout, "%s — %s\n", current.ID, summary)
				return err
			}
			_, err := fmt.Fprintf(a.stdout, "idle for %dms\n", idleFor.Milliseconds())
			return err
		}
		if a.now().Sub(start) >= timeout {
			if a.wantJSON {
				writeJSON(a.stdout, struct {
					OK      bool   `json:"ok"`
					Reason  string `json:"reason"`
					IdleMS  int64  `json:"idleMs"`
					Working bool   `json:"working"`
				}{false, "timeout", idleFor.Milliseconds(), current.Working}, false)
			} else {
				fmt.Fprintf(a.stderr, "timeout: still active after %dms (last %dms ago)\n", timeout.Milliseconds(), idleFor.Milliseconds())
			}
			return status(1)
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
