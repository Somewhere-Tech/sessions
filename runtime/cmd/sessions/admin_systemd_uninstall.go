package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// linuxUninstall is what uninstall did and then observed. It removes only a
// unit file that is exactly the definition install generates for this home
// (eligibleLinuxUnit), disables the unit name only when systemd loads it from
// that file, and it never stops, restarts, or signals a process or deletes
// state, history, credentials, or runtime bytes. daemon_stopped and
// state_preserved therefore describe this command's own actions; whether
// systemd or host policy later ends the daemon or its runners is not observed
// and not claimed. service_state and unit_file_state are systemd's answers
// afterwards.
type linuxUninstall struct {
	OK       bool   `json:"ok"`
	Code     int    `json:"code,omitempty"`
	Error    string `json:"error,omitempty"`
	Service  string `json:"service"`
	UnitPath string `json:"unit_path"`
	UnitFile string `json:"unit_file"` // removed, absent, kept, or not_removed
	// Removed keeps the shape v0.2.27 shipped, a path string, with accurate
	// meaning: the unit file this run removed, or empty when it removed none.
	Removed        string   `json:"removed"`
	DaemonAction   string   `json:"daemon_action"`
	DaemonStopped  bool     `json:"daemon_stopped"`
	StatePreserved bool     `json:"state_preserved"`
	ServiceState   string   `json:"service_state"`
	UnitFileState  string   `json:"unit_file_state"`
	Actions        []string `json:"actions"`
	Kept           []string `json:"kept"`
	Errors         []string `json:"errors,omitempty"`
	Next           []string `json:"next"`
}

// linuxUnitObservation is what systemd reported before uninstall changed
// anything: whether it has a definition loaded for the name, from which file,
// with which drop-ins, and the login enablement.
type linuxUnitObservation struct {
	loadState, unitFileState, fragmentPath string
	dropIns                                []string
}

func observeLinuxUnit(m *linuxServiceManager, name string) (linuxUnitObservation, error) {
	output, err := m.systemctl("show", name, "--property=LoadState,UnitFileState,FragmentPath,DropInPaths")
	properties := systemdProperties(output)
	unitFileState, reported := properties["UnitFileState"]
	if err != nil || properties["LoadState"] == "" || !reported {
		return linuxUnitObservation{}, errors.New(outputOrError(output, err))
	}
	return linuxUnitObservation{loadState: properties["LoadState"], unitFileState: normalizedUnitFileState(unitFileState),
		fragmentPath: properties["FragmentPath"], dropIns: strings.Fields(properties["DropInPaths"])}, nil
}

// Uninstall removes the generated Sessions login integration and nothing
// else. It is safe to repeat: a unit that is already gone only completes a reload an
// interrupted run left pending.
func (a *app) uninstallLinuxService(args []string) error {
	if len(args) != 0 {
		return fail(1, "usage: sessions uninstall")
	}
	m := a.linuxServices()
	if err := requireUserSystemd(m); err != nil {
		return err
	}
	name, err := linuxServiceName()
	if err != nil {
		return err
	}
	u := &linuxUninstall{Service: name, UnitPath: linuxUnitPath(a.home, name), DaemonAction: "none", StatePreserved: true,
		Actions: []string{}, Kept: []string{}, Next: []string{}}
	before, err := observeLinuxUnit(m, name)
	if err != nil {
		return fail(2, "could not observe %s (%s); nothing was disabled, removed, or reloaded and no process was signalled, so check the output of: %s, and retry",
			name, err, shellCommand("systemctl", "--user", "status", "--", name))
	}
	switch unit, kind := readLinuxUnitForUninstall(u.UnitPath, a.home); kind {
	case "absent":
		u.completeAbsent(m, before)
	case "eligible":
		u.removeEligible(m, before)
	default:
		u.keepUnrecognised(kind, unit)
	}
	u.ServiceState = observedServiceState(m, name)
	u.UnitFileState = observedUnitFileState(m, name)
	u.Kept = append(u.Kept, "no process was stopped, restarted, or signalled by this command; service_state is systemd's answer afterwards",
		"nothing was deleted: Sessions state, history, ledger, and credentials, and the staged runtime in "+linuxRuntimeRoot(a.home)+", are untouched")
	return a.reportLinuxUninstall(u)
}

// readLinuxUnitForUninstall classifies the file at the unit path. install
// always writes a regular file, so a symbolic link there -- a mask, or a
// systemctl link -- is not its output.
func readLinuxUnitForUninstall(path, home string) (string, string) {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", "absent"
	case err != nil:
		return err.Error(), "unreadable"
	case !info.Mode().IsRegular():
		return info.Mode().Type().String(), "not_regular"
	}
	unit, err := os.ReadFile(path)
	if err != nil {
		return err.Error(), "unreadable"
	}
	if eligibleLinuxUnit(unit, home) {
		return string(unit), "eligible"
	}
	return string(unit), "unrecognised"
}

// completeAbsent handles a unit path with no file. systemd may still hold the
// definition an interrupted uninstall removed; only that is reloaded. A unit
// of the same name from another directory was not written by install.
func (u *linuxUninstall) completeAbsent(m *linuxServiceManager, before linuxUnitObservation) {
	u.UnitFile = "absent"
	u.Actions = append(u.Actions, "found no unit file at "+u.UnitPath+", so nothing was disabled or removed")
	switch {
	case before.loadState != "not-found" && before.fragmentPath == u.UnitPath:
		u.reload(m, "reloaded user service definitions: systemd still held the removed definition")
	case before.fragmentPath != "":
		u.Kept = append(u.Kept, fmt.Sprintf("%s is provided by %s (login enablement %s), which sessions install did not write; it was left as found",
			u.Service, before.fragmentPath, describeUnitFileState(before.unitFileState)))
	}
	u.OK = len(u.Errors) == 0
}

// removeEligible disables, removes, and reloads in dependency order: disable
// needs the file to resolve the unit's links, and each later step runs only
// once the one before it has happened. disable acts on the unit name, not on
// a file, so it runs only when systemd loads that name from this file.
func (u *linuxUninstall) removeEligible(m *linuxServiceManager, before linuxUnitObservation) {
	u.UnitFile = "not_removed"
	if len(before.dropIns) > 0 {
		u.Kept = append(u.Kept, "drop-ins "+strings.Join(before.dropIns, ", ")+", which sessions install never writes")
	}
	if !sameUnitFile(before.fragmentPath, u.UnitPath) {
		u.skipShadowedDisable(before)
	} else if !u.disable(m, before) {
		return
	}
	if err := os.Remove(u.UnitPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		u.failed("remove "+u.UnitPath+": "+err.Error(),
			"the unit file remains (any disable already run is listed under actions); fix the directory's permissions and rerun sessions uninstall, or run: "+
				shellCommand("rm", "--", u.UnitPath)+" && "+shellCommand("systemctl", "--user", "daemon-reload"))
		return
	}
	u.UnitFile, u.Removed = "removed", u.UnitPath
	u.Actions = append(u.Actions, "removed "+u.UnitPath+", the definition sessions install generates for this home's managed runtime")
	u.reload(m, "reloaded user service definitions")
	u.OK = len(u.Errors) == 0
}

// skipShadowedDisable records why disable was not run: systemd resolves the
// name to another file or a mask, so a disable by name would change that
// unit's enablement. Nothing here claims that enablement is unchanged; it is
// reported before and, as unit_file_state, after.
func (u *linuxUninstall) skipShadowedDisable(before linuxUnitObservation) {
	source := "from " + before.fragmentPath
	if before.fragmentPath == "" {
		source = "with LoadState " + before.loadState + " and no unit file"
	}
	u.Actions = append(u.Actions, "did not run systemctl --user disable "+u.Service+": systemd loads that name "+source+
		", not from the Sessions unit, and disable acts on the name (login enablement before: "+describeUnitFileState(before.unitFileState)+")")
	u.Kept = append(u.Kept, "the unit systemd loads as "+u.Service+" ("+source+"); sessions install did not write it and this command neither edited nor disabled it")
	u.Next = append(u.Next, "if "+u.Service+" should no longer start at login, review the output of "+shellCommand("systemctl", "--user", "cat", "--", u.Service)+
		" and "+shellCommand("systemctl", "--user", "is-enabled", "--", u.Service)+", then disable it yourself")
}

func (u *linuxUninstall) disable(m *linuxServiceManager, before linuxUnitObservation) bool {
	if output, err := m.systemctl("disable", u.Service); err != nil {
		u.failed("systemctl --user disable "+u.Service+": "+outputOrError(output, err),
			"nothing was removed; check the output of "+shellCommand("systemctl", "--user", "is-enabled", "--", u.Service)+", then rerun sessions uninstall")
		return false
	}
	u.Actions = append(u.Actions, "ran systemctl --user disable "+u.Service+", which removes its persistent login enablement (before: "+describeUnitFileState(before.unitFileState)+")")
	if before.unitFileState == "enabled-runtime" {
		u.Kept = append(u.Kept, "runtime-only enablement (systemctl --user enable --runtime, under /run): a plain disable does not change it and Sessions never creates it; systemd drops it at reboot, or run: "+shellCommand("systemctl", "--user", "disable", "--runtime", "--", u.Service))
	}
	return true
}

// sameUnitFile reports whether systemd's FragmentPath is the Sessions unit
// file, allowing for a symlinked directory on the way to it.
func sameUnitFile(fragment, path string) bool {
	if fragment == "" {
		return false
	}
	if fragment == path {
		return true
	}
	a, errA := os.Stat(fragment)
	b, errB := os.Stat(path)
	return errA == nil && errB == nil && os.SameFile(a, b)
}

func (u *linuxUninstall) reload(m *linuxServiceManager, done string) {
	if output, err := m.systemctl("daemon-reload"); err != nil {
		u.failed("systemctl --user daemon-reload: "+outputOrError(output, err),
			"the unit file is gone but systemd still holds its definition; rerun sessions uninstall, or run: "+shellCommand("systemctl", "--user", "daemon-reload"))
		return
	}
	u.Actions = append(u.Actions, done)
}

func (u *linuxUninstall) keepUnrecognised(kind, detail string) {
	u.UnitFile = "kept"
	if kind == "not_regular" {
		// No Sessions-written file can be present: install replaces whatever is
		// at this path with a regular file.
		u.OK = true
		u.Kept = append(u.Kept, fmt.Sprintf("%s is a %s (a mask or a systemctl link, for example), which sessions install never writes; it was left as found and %s was not disabled",
			u.UnitPath, detail, u.Service))
		return
	}
	reason := "is not exactly the definition sessions install generates for this home's managed runtime (an edited ExecStart, environment, or setting, or another program's unit)"
	if kind == "unreadable" {
		reason = "could not be read (" + detail + "), so Sessions cannot check it against the definition install generates"
	}
	u.failed(u.UnitPath+" "+reason+"; it was left as found and "+u.Service+" was not disabled",
		"if it is the Sessions service, remove it yourself by running: "+shellCommand("systemctl", "--user", "disable", "--", u.Service)+" && "+
			shellCommand("rm", "--", u.UnitPath)+" && "+shellCommand("systemctl", "--user", "daemon-reload"))
}

// shellCommand renders a command for a person to paste into a POSIX shell:
// an argument with any character outside shellSafe is single-quoted, with
// each apostrophe closed, backslash-escaped, and reopened, so spaces, quotes,
// $, backticks, and semicolons in a path stay one literal argument. This is shell quoting, not
// systemdQuote's unit-file escaping. Callers put -- before any path or unit
// name so a leading dash cannot become an option.
func shellCommand(arguments ...string) string {
	quoted := make([]string, len(arguments))
	for index, argument := range arguments {
		quoted[index] = argument
		if strings.ContainsFunc(argument, func(r rune) bool { return !strings.ContainsRune(shellSafe, r) }) || argument == "" {
			quoted[index] = "'" + strings.ReplaceAll(argument, "'", `'\''`) + "'"
		}
	}
	return strings.Join(quoted, " ")
}

// shellSafe are characters a POSIX shell gives no special meaning; a word of
// only these is left bare so simple commands stay readable.
const shellSafe = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789@%+,./:_-"

func (u *linuxUninstall) failed(err, next string) {
	u.Errors = append(u.Errors, err)
	u.Next = append(u.Next, next)
}

func (a *app) reportLinuxUninstall(u *linuxUninstall) error {
	if !u.OK {
		u.Code = 2
		u.Error = fmt.Sprintf("uninstall %s is incomplete: %s; next: %s", u.Service, strings.Join(u.Errors, "; "), strings.Join(u.Next, "; "))
		if !a.wantJSON {
			return fail(2, "%s; done: %s; kept: %s", u.Error, strings.Join(u.Actions, "; "), strings.Join(u.Kept, "; "))
		}
		_ = writeJSON(a.stdout, u, true)
		return status(2)
	}
	if a.wantJSON {
		return writeJSON(a.stdout, u, true)
	}
	headline := map[string]string{"removed": "Removed the login integration for ", "absent": "No Sessions login integration was installed for ",
		"kept": "Left the unit file Sessions did not write in place for "}[u.UnitFile]
	fmt.Fprintf(a.stdout, "%s%s; service state: %s; login enablement: %s.\n", headline, u.Service, u.ServiceState, describeUnitFileState(u.UnitFileState))
	for _, line := range u.Actions {
		fmt.Fprintln(a.stdout, "  done: "+line)
	}
	for _, line := range u.Kept {
		fmt.Fprintln(a.stdout, "  kept: "+line)
	}
	return nil
}

// eligibleLinuxUnit reports whether unit is exactly the definition install
// generates for this home: byte-identical to linuxServiceUnit's output, with
// ExecStart running sessionsd from a content-addressed directory under this
// home's managed runtime root, and only the environment linuxServiceConfig
// writes, in its order -- HOME this home, SESSIONS_RUNNER the staged runner
// beside that sessionsd, then optional keys it passes through. An edited
// ExecStart, environment, or setting does not match. This checks what the file
// is, not who wrote it: a byte-identical copy runs the same staged runtime and
// is the same integration.
func eligibleLinuxUnit(unit []byte, home string) bool {
	config, ok := parseLinuxUnit(unit)
	staged := filepath.Dir(config.Daemon)
	if !ok || filepath.Base(config.Daemon) != "sessionsd" || filepath.Dir(staged) != linuxRuntimeRoot(home) || !contentAddress(filepath.Base(staged)) {
		return false
	}
	required := []string{"HOME", "PATH", "SESSIONS_HOST", "SESSIONS_PORT", "SESSIONS_RUNNER"}
	if len(config.Env) < len(required) || config.Env[0].Value != home || config.Env[4].Value != filepath.Join(staged, "sessions-runner") {
		return false
	}
	for index, key := range required {
		if config.Env[index].Key != key {
			return false
		}
	}
	optional := linuxOptionalEnvironment
	for _, entry := range config.Env[len(required):] {
		index := slices.Index(optional, entry.Key)
		if index < 0 {
			return false
		}
		optional = optional[index+1:]
	}
	return true
}

// parseLinuxUnit reads back the template's variable parts, ExecStart and the
// Environment lines, and succeeds only if regenerating from them reproduces
// the file byte for byte. A misread can only fail that comparison.
func parseLinuxUnit(unit []byte) (linuxServiceConfig, bool) {
	var config linuxServiceConfig
	for _, line := range strings.Split(string(unit), "\n") {
		if value, ok := strings.CutPrefix(line, "ExecStart="); ok {
			daemon, ok := systemdUnquote(value)
			if !ok {
				return config, false
			}
			config.Daemon = strings.ReplaceAll(daemon, "$$", "$")
		} else if value, ok := strings.CutPrefix(line, "Environment="); ok {
			entry, ok := systemdUnquote(value)
			key, entryValue, found := strings.Cut(entry, "=")
			if !ok || !found {
				return config, false
			}
			config.Env = append(config.Env, plistEnvironment{Key: key, Value: entryValue})
		}
	}
	return config, config.Daemon != "" && linuxServiceUnit(config) == string(unit)
}

// contentAddress matches the directory names stageLinuxRuntime creates: the
// lower-case hex SHA-256 of the staged binaries.
func contentAddress(name string) bool {
	return len(name) == 64 && strings.Trim(name, "0123456789abcdef") == ""
}

// systemdUnquote inverts systemdQuote.
func systemdUnquote(quoted string) (string, bool) {
	if len(quoted) < 2 || quoted[0] != '"' || quoted[len(quoted)-1] != '"' {
		return "", false
	}
	inner, value := quoted[1:len(quoted)-1], strings.Builder{}
	for index := 0; index < len(inner); index++ {
		if inner[index] != '\\' {
			value.WriteByte(inner[index])
			continue
		}
		if index++; index == len(inner) {
			return "", false
		}
		escaped, known := map[byte]byte{'\\': '\\', '"': '"', 'n': '\n', 'r': '\r'}[inner[index]]
		if !known {
			return "", false
		}
		value.WriteByte(escaped)
	}
	return strings.ReplaceAll(value.String(), "%%", "%"), true
}
