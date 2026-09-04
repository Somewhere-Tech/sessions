package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/creack/pty"
	"github.com/somewhere-tech/sessions/runtime/internal/localnetwork"
	sessionstate "github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

type doctorRow struct {
	ID       string `json:"id"`
	Tool     string `json:"tool"`
	Size     string `json:"size"`
	QoS      string `json:"qos"`
	Spawn    string `json:"spawn"`
	Recovery bool   `json:"needs_recovery,omitempty"`
	Lost     bool   `json:"lost,omitempty"`
	Action   string `json:"action,omitempty"`
	OK       bool   `json:"ok"`
}

// doctorMirrorRow is one stored conversation that reports having stopped
// recording. It is deliberately not a column on doctorRow: that table describes
// live runners, and the conversations most at risk belong to sessions that
// ended long ago and have no runner left to put in a row.
type doctorMirrorRow struct {
	ID     string `json:"id"`
	Detail string `json:"detail"`
}

const legacyRunnerLabelPrefix = "tech.pretty-pty.runner."

func (a *app) cmdDoctor() error {
	localRuntime := a.api.localToken && a.api.pathPrefix == ""
	if localRuntime {
		if err := ptyPreflight(); err != nil {
			return err
		}
	}
	sessions, err := a.listSessions(false)
	if err != nil {
		return err
	}
	lan := a.doctorLocalNetwork()
	var deep any
	if response, requestErr := a.api.request(context.Background(), "GET", "/api/health/deep", nil, 0); requestErr == nil && response.status < 400 {
		_ = json.Unmarshal(response.body, &deep)
	}
	processTypePattern := regexp.MustCompile(`<key>ProcessType</key>\s*<string>([^<]+)</string>`)
	rows := make([]doctorRow, 0, len(sessions))
	for _, value := range sessions {
		rows = append(rows, a.doctorRunnerRow(value, localRuntime, processTypePattern))
	}
	damagedMirrors := []doctorMirrorRow(nil)
	if localRuntime {
		damagedMirrors = damagedTranscriptMirrors()
	}
	if a.wantJSON {
		return a.writeDoctorJSON(deep, lan, rows, damagedMirrors)
	}
	if deepMap, ok := deep.(map[string]any); ok {
		fmt.Fprintf(a.stdout, "daemon: %s sessions, discovering=%s, uptime=%ss\n\n",
			jsonScalar(deepMap["sessionsLoaded"]), jsonScalar(deepMap["discovering"]), jsonScalar(deepMap["uptimeSec"]))
		if restore, ok := deepMap["restore"].(map[string]any); ok {
			if pending, ok := restore["pending"].(float64); ok && pending > 0 {
				fmt.Fprintf(a.stdout, "restore: %.0f session(s) stayed paused after reboot; their history is preserved for explicit recovery\n\n", pending)
			}
		}
	}
	writeDoctorLocalNetwork(a.stdout, lan)
	fmt.Fprintf(a.stdout, "%s%s%s%s%sSTATUS\n",
		fixedWidth("ID", 10), fixedWidth("TOOL", 8), fixedWidth("SIZE", 10), fixedWidth("QoS", 13), fixedWidth("SPAWN", 10))
	runnerFaults := 0
	recoveryRows := 0
	lostRows := 0
	for _, row := range rows {
		statusText := "ok"
		if row.Recovery {
			statusText = "⚠ resume required"
			recoveryRows++
		} else if row.Lost {
			statusText = "⚠ lost — " + row.Action
			lostRows++
		} else if !row.OK {
			statusText = "⚠ needs recreate"
			runnerFaults++
		}
		fmt.Fprintf(a.stdout, "%s%s%s%s%s%s\n",
			fixedWidth(prefixString(row.ID, 8), 10), fixedWidth(shortToolName(row.Tool), 8),
			fixedWidth(row.Size, 10), fixedWidth(row.QoS, 13), fixedWidth(row.Spawn, 10), statusText)
	}
	recovery := max(recoveryRows, restorePendingFromHealth(deep))
	attention := recovery + lostRows + runnerFaults
	fmt.Fprintf(a.stdout, "\n%d session(s) need attention", attention)
	if attention > 0 {
		io.WriteString(a.stdout, ": ")
		if recovery > 0 {
			fmt.Fprintf(a.stdout, "%d paused after reboot — resume only the sessions you want with `sessions resume <id>`", recovery)
		}
		if lostRows > 0 {
			if recovery > 0 {
				io.WriteString(a.stdout, "; ")
			}
			fmt.Fprintf(a.stdout, "%d lost runner(s) — use each row's action to close or continue it", lostRows)
		}
		if runnerFaults > 0 {
			if recovery > 0 || lostRows > 0 {
				io.WriteString(a.stdout, "; ")
			}
			fmt.Fprintf(a.stdout, "%d runner fault(s) %s", runnerFaults, doctorUnhealthyAdvice())
		}
		io.WriteString(a.stdout, "\n")
		a.exitCode = 1
	} else {
		io.WriteString(a.stdout, " "+doctorHealthySummary()+"\n")
	}
	a.writeDoctorMirrorHealth(damagedMirrors)
	return nil
}

func (a *app) doctorLocalNetwork() any {
	var lan any
	response, err := a.api.request(context.Background(), "GET", "/api/lan", nil, 0)
	if err == nil && response.status < 400 {
		_ = json.Unmarshal(response.body, &lan)
	}
	return lan
}

func (a *app) writeDoctorJSON(deep, lan any, rows []doctorRow, mirrors []doctorMirrorRow) error {
	return writeJSON(a.stdout, struct {
		Daemon         any               `json:"daemon"`
		LocalNetwork   any               `json:"local_network"`
		Sessions       []doctorRow       `json:"sessions"`
		DamagedMirrors []doctorMirrorRow `json:"damaged_conversations,omitempty"`
	}{deep, lan, rows, mirrors}, true)
}

func writeDoctorLocalNetwork(writer io.Writer, state any) {
	lan, ok := state.(map[string]any)
	if !ok {
		return
	}
	permission, ok := lan["permission"].(map[string]any)
	if !ok {
		return
	}
	status, _ := permission["status"].(string)
	switch status {
	case "denied":
		fmt.Fprintf(writer, "local network: denied — %s\n\n", localnetwork.Message)
	case "granted":
		fmt.Fprint(writer, "local network: granted\n\n")
	case "not-yet-asked":
		fmt.Fprint(writer, "local network: not yet asked; open Fleet in Sessions to choose when macOS asks\n\n")
	}
}

// doctorRunnerRow inspects process and launch-service state only when the CLI
// and daemon are on the same machine. A PID and plist path are host-local
// identities; applying the MacBook's ps/LaunchAgents results to a Mini made a
// healthy remote fleet look entirely broken.
func (a *app) doctorRunnerRow(value session, localRuntime bool, processTypePattern *regexp.Regexp) doctorRow {
	row := doctorRow{
		ID: value.ID, Tool: toolOfSession(value), Size: fmt.Sprintf("%dx%d", value.Cols, value.Rows),
		QoS: probeNotApplicable, Spawn: probeNotApplicable,
	}
	if value.UnreachableReason == "restart-restore-pending" {
		row.Spawn = "paused"
		row.Recovery = true
		return row
	}
	if !localRuntime {
		if value.RunnerGone {
			row.Lost = true
			row.Action = sessionRecoveryCommand(value)
			return row
		}
		row.OK = true
		return row
	}
	// Per-session service QoS is a launchd concept. The Windows adapter is a
	// logon supervisor with no ProcessType, so the probe does not apply there.
	if runtime.GOOS == "darwin" {
		row.QoS = runnerQoS(a.home, value.ID, processTypePattern)
	}
	row.Spawn = "dead?"
	if value.PID != 0 {
		// The runner is intentionally independent of the daemon and app and may
		// be re-parented after either updates. Inspect the runner itself.
		row.Spawn = probeNotApplicable
		if canProbeProcessCommand() {
			row.Spawn = runnerSpawn(value.PID, psField)
		}
	}
	row.OK = doctorRowOK(row.QoS, row.Spawn)
	if value.Unreachable && value.UnreachableReason == "runner-lost" && row.Spawn != "native" {
		row.Lost = true
		value.RunnerGone = true
		row.Action = sessionRecoveryCommand(value)
		row.OK = false
	}
	return row
}

func restorePendingFromHealth(deep any) int {
	health, ok := deep.(map[string]any)
	if !ok {
		return 0
	}
	restore, ok := health["restore"].(map[string]any)
	if !ok {
		return 0
	}
	switch pending := restore["pending"].(type) {
	case float64:
		return max(int(pending), 0)
	case int:
		return max(pending, 0)
	default:
		return 0
	}
}

// writeDoctorMirrorHealth reports stored conversations that are missing
// records. Doctor is where someone goes when something feels wrong, and a
// conversation that came back shorter than they remember is one of the few
// things that can feel wrong without any session looking unhealthy at all.
//
// Silence here means "no stored conversation reports a problem", never "every
// conversation is complete": a mirror with no readable sidecar has unknown
// health and is not counted, so this must not be written as an all-clear.
func (a *app) writeDoctorMirrorHealth(damaged []doctorMirrorRow) {
	if len(damaged) == 0 {
		return
	}
	fmt.Fprintf(a.stdout, "\n%d stored %s missing records:\n",
		len(damaged), map[bool]string{true: "conversation is", false: "conversations are"}[len(damaged) == 1])
	for _, row := range damaged {
		fmt.Fprintf(a.stdout, "  %s  %s\n", prefixString(row.ID, 8), row.Detail)
	}
	// Nothing recreates a conversation, so the advice cannot be doctor's usual
	// "recreate the session". It has to be the two things that are actually
	// still open: rescue the provider's copy while it exists, and stop the
	// cause before the next session loses records the same way.
	io.WriteString(a.stdout,
		"Sessions' own copy of each of these stopped part way and cannot be repaired; a copy is only ever appended to. "+
			"Run `sessions source <id>` to see which file that conversation is now read from, and save the provider's own "+
			"transcript with `sessions source <id> --raw` while the provider still has it. A copy that failed on write means "+
			"the runner state directory was full or unwritable — fix that first, or live sessions keep losing records now.\n")
	// Deliberately not exit 1. Doctor's non-zero already means one thing --
	// sessions need recreate -- and recreating them clears it. A copy that
	// stopped recording can never be cleared: it is append-only, nothing
	// rewrites it, and rescuing the provider's transcript does not change what
	// Sessions holds. Failing here would make doctor permanently red on a host
	// that has ever lost records, with no action that turns it green, which is
	// how a health check gets ignored. The loss is reported in full above and
	// in --json; the exit status stays a question about what can be fixed.
}

// damagedTranscriptMirrors scans the runner state directory rather than the
// daemon's session list, because an ended session still has its stored
// conversation and is exactly the case where nobody would notice the loss.
// Every failure here is silent: doctor's job is to report what it can probe,
// not to fail because one directory could not be read.
func damagedTranscriptMirrors() []doctorMirrorRow {
	config, err := sessionstate.ConfigFromEnv()
	if err != nil || config.RunnerStateDir == "" {
		return nil
	}
	entries, err := os.ReadDir(config.RunnerStateDir)
	if err != nil {
		return nil
	}
	damaged := make([]doctorMirrorRow, 0)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, watch.TranscriptMirrorSuffix) {
			continue
		}
		mirrorPath := filepath.Join(config.RunnerStateDir, name)
		if !watch.TranscriptMirrorUsable(mirrorPath) {
			continue
		}
		if health := watch.ReadTranscriptMirrorHealth(mirrorPath); health.Degraded() {
			damaged = append(damaged, doctorMirrorRow{
				ID:     strings.TrimSuffix(name, watch.TranscriptMirrorSuffix),
				Detail: health.Detail(),
			})
		}
	}
	sort.Slice(damaged, func(i, j int) bool { return damaged[i].ID < damaged[j].ID })
	return damaged
}

// probeNotApplicable marks a column whose probe has no meaning on this host,
// as opposed to a probe that ran and found nothing.
const probeNotApplicable = "n/a"

// ptyPreflight exercises the Unix PTY adapter. Windows uses ConPTY and has no
// /usr/bin/true, so running this there reported a broken terminal and told the
// user to install Xcode command line tools on a machine that has no Xcode.
func ptyPreflight() error {
	if runtime.GOOS == "windows" {
		return nil
	}
	hint := ""
	if runtime.GOOS == "darwin" {
		hint = "; run xcode-select --install"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/usr/bin/true")
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Cols: 80, Rows: 24})
	if err != nil {
		return fail(2, "PTY preflight failed: %s%s", err, hint)
	}
	// Closing the PTY master before the tiny child has actually exited sends
	// SIGHUP on macOS. That made doctor diagnose a broken PTY on otherwise
	// healthy installs. Wait for the child first; the context still bounds the
	// probe, and the master is closed immediately afterwards.
	waitErr := command.Wait()
	_ = terminal.Close()
	if waitErr != nil && ctx.Err() == nil {
		return fail(2, "PTY preflight failed: %s%s", waitErr, hint)
	}
	if ctx.Err() != nil {
		return fail(2, "PTY preflight failed: test PTY timed out%s", hint)
	}
	return nil
}

// canProbeProcessCommand reports whether this host can be asked what command a
// pid is running. Windows has no ps, and the Windows host evidence contract
// deliberately does not collect process command lines.
func canProbeProcessCommand() bool {
	return runtime.GOOS != "windows"
}

// doctorRowOK treats a skipped probe as neutral: only a probe that actually ran
// and found a problem marks a session as needing recreate.
func doctorRowOK(qos, spawn string) bool {
	if spawn != "native" && spawn != probeNotApplicable {
		return false
	}
	return qos == "Interactive" || qos == probeNotApplicable
}

func doctorUnhealthyAdvice() string {
	if runtime.GOOS == "darwin" {
		return "(the runner is not the shipped sessions-runner and/or its service QoS is throttled — recreate those sessions, or restart Sessions, to get the current runtime)."
	}
	return "(the runner is not the shipped sessions-runner — recreate those sessions to get the current runtime)."
}

func doctorHealthySummary() string {
	if runtime.GOOS == "darwin" {
		return "— all healthy (Interactive QoS, native runner)."
	}
	return "— all healthy (native runner)."
}

func runnerQoS(home, id string, processTypePattern *regexp.Regexp) string {
	for _, plistPath := range runnerPlistPaths(home, id) {
		encoded, err := os.ReadFile(plistPath)
		if err != nil {
			continue
		}
		match := processTypePattern.FindSubmatch(encoded)
		if match != nil {
			return string(match[1])
		}
		return "none"
	}
	return "no-plist"
}

// runnerPlistPaths is macOS-only; its callers are guarded by GOOS. The legacy
// label remains in the list for the documented adoption window.
func runnerPlistPaths(home, id string) []string {
	launchAgents := filepath.Join(home, "Library", "LaunchAgents")
	return []string{
		filepath.Join(launchAgents, "tech.somewhere.sessions.runner."+id+".plist"),
		filepath.Join(launchAgents, legacyRunnerLabelPrefix+id+".plist"),
	}
}

// classifyRunnerSpawn names what is actually running as the session's runner.
// Only the shipped sessions-runner is native; every other command is reported
// as "other" so an unrelated process cannot be mislabeled as healthy.
func classifyRunnerSpawn(runnerCommand string) string {
	switch {
	case strings.Contains(runnerCommand, "sessions-runner"):
		return "native"
	case runnerCommand != "":
		return "other"
	default:
		return "dead?"
	}
}

// runnerSpawn resolves the process that owns the provider child recorded in
// session metadata. PTY sessions record the provider PID (Claude, Codex, or a
// shell), whose direct parent is sessions-runner; structured sessions record
// sessions-runner itself. Treating the child command as the runner made doctor
// falsely condemn every healthy PTY session.
func runnerSpawn(pid int, field func(string, int) string) string {
	command := field("command=", pid)
	if classified := classifyRunnerSpawn(command); classified == "native" || classified == "dead?" {
		return classified
	}
	parent, err := strconv.Atoi(strings.TrimSpace(field("ppid=", pid)))
	if err != nil || parent <= 0 {
		return "other"
	}
	return classifyRunnerSpawn(field("command=", parent))
}

func psField(format string, pid int) string {
	output, err := exec.Command("ps", "-o", format, "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func fixedWidth(value string, width int) string {
	if jsLength(value) >= width {
		units := 0
		var truncated strings.Builder
		for _, char := range value {
			charUnits := jsLength(string(char))
			if units+charUnits > width-1 {
				break
			}
			truncated.WriteRune(char)
			units += charUnits
		}
		value = truncated.String()
	}
	return value + strings.Repeat(" ", width-jsLength(value))
}

func jsonScalar(value any) string {
	switch typed := value.(type) {
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
	}
	return fmt.Sprint(value)
}
