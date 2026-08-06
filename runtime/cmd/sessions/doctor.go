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
	"strconv"
	"strings"
	"time"

	"github.com/creack/pty"
)

type doctorRow struct {
	ID    string `json:"id"`
	Tool  string `json:"tool"`
	Size  string `json:"size"`
	QoS   string `json:"qos"`
	Spawn string `json:"spawn"`
	OK    bool   `json:"ok"`
}

const legacyRunnerLabelPrefix = "tech.pretty-pty.runner."

func (a *app) cmdDoctor() error {
	if err := ptyPreflight(); err != nil {
		return err
	}
	sessions, err := a.listSessions(false)
	if err != nil {
		return err
	}
	var deep any
	if response, requestErr := a.api.request(context.Background(), "GET", "/api/health/deep", nil, 0); requestErr == nil && response.status < 400 {
		_ = json.Unmarshal(response.body, &deep)
	}
	processTypePattern := regexp.MustCompile(`<key>ProcessType</key>\s*<string>([^<]+)</string>`)
	rows := make([]doctorRow, 0, len(sessions))
	for _, value := range sessions {
		// Per-session service QoS is a launchd concept. The Windows adapter is
		// a logon supervisor with no ProcessType, so reporting "no-plist"
		// there would invent a fault; say the probe does not apply instead.
		qos := probeNotApplicable
		if runtime.GOOS == "darwin" {
			qos = runnerQoS(a.home, value.ID, processTypePattern)
		}
		spawn := "dead?"
		if value.PID != 0 {
			// The runner is intentionally independent of the daemon and the
			// app. It may be re-parented after either one updates, so its
			// parent command is not evidence of how the runner itself was
			// launched. Inspect the durable runner process instead.
			spawn = probeNotApplicable
			if canProbeProcessCommand() {
				spawn = classifyRunnerSpawn(psField("command=", value.PID))
			}
		}
		rows = append(rows, doctorRow{
			ID: value.ID, Tool: toolOfSession(value), Size: fmt.Sprintf("%dx%d", value.Cols, value.Rows),
			QoS: qos, Spawn: spawn, OK: doctorRowOK(qos, spawn),
		})
	}
	if a.wantJSON {
		return writeJSON(a.stdout, struct {
			Daemon   any         `json:"daemon"`
			Sessions []doctorRow `json:"sessions"`
		}{deep, rows}, true)
	}
	if deepMap, ok := deep.(map[string]any); ok {
		fmt.Fprintf(a.stdout, "daemon: %s sessions, discovering=%s, uptime=%ss\n\n",
			jsonScalar(deepMap["sessionsLoaded"]), jsonScalar(deepMap["discovering"]), jsonScalar(deepMap["uptimeSec"]))
	}
	fmt.Fprintf(a.stdout, "%s%s%s%s%sSTATUS\n",
		fixedWidth("ID", 10), fixedWidth("TOOL", 8), fixedWidth("SIZE", 10), fixedWidth("QoS", 13), fixedWidth("SPAWN", 10))
	bad := 0
	for _, row := range rows {
		statusText := "ok"
		if !row.OK {
			statusText = "⚠ needs recreate"
			bad++
		}
		fmt.Fprintf(a.stdout, "%s%s%s%s%s%s\n",
			fixedWidth(prefixString(row.ID, 8), 10), fixedWidth(shortToolName(row.Tool), 8),
			fixedWidth(row.Size, 10), fixedWidth(row.QoS, 13), fixedWidth(row.Spawn, 10), statusText)
	}
	fmt.Fprintf(a.stdout, "\n%d of %d sessions need recreate ", bad, len(rows))
	if bad > 0 {
		io.WriteString(a.stdout, doctorUnhealthyAdvice()+"\n")
		a.exitCode = 1
	} else {
		io.WriteString(a.stdout, doctorHealthySummary()+"\n")
	}
	return nil
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
// The retired Node runtime's "dist"/"tsx-SLOW" classifications are gone: a
// shipped Go install cannot spawn dist/runner.js or tsx, so those buckets could
// only mislabel some unrelated process as a healthy or a slow Sessions runner.
// Anything that is not the shipped sessions-runner is now reported as "other",
// which is what it is.
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
