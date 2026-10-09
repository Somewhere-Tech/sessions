package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// linuxUnitBaseline is what the installer found before it changed anything:
// the exact unit bytes and mode, systemd's UnitFileState, and whether its
// daemon was running. Recovery restores exactly this and nothing it guessed.
type linuxUnitBaseline struct {
	unit    []byte
	mode    fs.FileMode
	existed bool
	// unitFileState is systemd's UnitFileState: enabled, enabled-runtime,
	// disabled, empty for a unit with no file, or another systemd state.
	unitFileState string
	active        bool
}

func readLinuxUnitBaseline(m *linuxServiceManager, config linuxServiceConfig) (linuxUnitBaseline, error) {
	var baseline linuxUnitBaseline
	unit, err := os.ReadFile(config.Path)
	switch {
	case err == nil:
		info, statErr := os.Stat(config.Path)
		if statErr != nil {
			return baseline, fail(2, "read %s: %v; the service definition was not changed", config.Path, statErr)
		}
		baseline.unit, baseline.mode, baseline.existed = unit, info.Mode().Perm(), true
	case !errors.Is(err, fs.ErrNotExist):
		return baseline, fail(2, "read the existing service definition %s: %v; the service definition was not changed, so fix its permissions and retry", config.Path, err)
	}
	// show answers for an absent unit too (LoadState=not-found, inactive), so
	// an error or a missing ActiveState is a failed observation -- never
	// evidence that no daemon is running.
	output, err := m.systemctl("show", config.Name, "--property=ActiveState,UnitFileState")
	properties := systemdProperties(output)
	state := properties["ActiveState"]
	unitFileState, unitFileReported := properties["UnitFileState"]
	if err != nil || state == "" || !unitFileReported {
		return baseline, fail(2, "could not observe whether %s is running or enabled (%s); the service definition was not changed and no daemon was started or stopped (the staged runtime at %s is kept), so check systemctl --user status %s and retry",
			config.Name, outputOrError(output, err), filepath.Dir(config.Daemon), config.Name)
	}
	baseline.active = state != "inactive" && state != "failed"
	baseline.unitFileState = normalizedUnitFileState(unitFileState)
	return baseline, nil
}

// systemdProperties reads systemctl show's KEY=VALUE lines. It is not a unit
// file parser: these are the values systemd itself resolved.
func systemdProperties(output []byte) map[string]string {
	properties := make(map[string]string)
	for _, line := range strings.Split(string(output), "\n") {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			properties[key] = value
		}
	}
	return properties
}

// effectiveKillMode is the stop policy systemd reports for the loaded unit,
// after drop-ins and every override it applies.
func effectiveKillMode(m *linuxServiceManager, name string) (string, error) {
	output, err := m.systemctl("show", name, "--property=KillMode", "--value")
	if err != nil {
		return "unknown", errors.New(outputOrError(output, err))
	}
	if mode := strings.TrimSpace(string(output)); mode != "" {
		return mode, nil
	}
	return "unknown", errors.New("systemd reported no KillMode")
}

// linuxInstallStepError names the installer step that failed. daemonTouched
// marks a start, restart, or health failure: only those may have replaced or
// started a daemon process.
type linuxInstallStepError struct {
	step          string
	detail        string
	daemonTouched bool
}

// linuxInstallRecovery is what recovery did and what it then observed, kept
// apart: a restored file is not a loaded definition, an active unit is not a
// healthy daemon, and a healthy endpoint does not identify which runtime
// binary answered, so runtime_identity is never claimed.
type linuxInstallRecovery struct {
	Outcome         string   `json:"outcome"`
	Configuration   string   `json:"configuration"` // restored, removed, or not_restored
	Loaded          bool     `json:"loaded"`        // daemon-reload succeeded after the file was restored
	KillMode        string   `json:"kill_mode,omitempty"`
	UnitFileState   string   `json:"unit_file_state"` // systemd's login enablement after recovery, or unknown
	ServiceState    string   `json:"service_state"`   // systemctl is-active after recovery, or unknown
	Healthy         *bool    `json:"healthy"`         // health check after a recovery restart; null when none ran
	RuntimeIdentity string   `json:"runtime_identity"`
	Actions         []string `json:"actions"`
	Errors          []string `json:"errors,omitempty"`
	Next            []string `json:"next"`
	PreviousUnit    string   `json:"previous_unit,omitempty"`
}

func (r *linuxInstallRecovery) run(m *linuxServiceManager, done string, arguments ...string) bool {
	if output, err := m.systemctl(arguments...); err != nil {
		r.Errors = append(r.Errors, fmt.Sprintf("systemctl --user %s: %s", strings.Join(arguments, " "), outputOrError(output, err)))
		return false
	}
	r.Actions = append(r.Actions, done)
	return true
}

func (r *linuxInstallRecovery) failed(err, next string) {
	r.Errors = append(r.Errors, err)
	r.Next = append(r.Next, next)
}

// recoverLinuxInstall returns the service to its baseline after a failed
// install. Each step depends on the one before it actually succeeding: the
// previous runtime is activated only once its file is back and systemd has
// loaded it. Sessions never signals a runner itself, and it issues a stop or
// restart only after systemd reports the unit's effective KillMode=process,
// so a drop-in or hand edit that widens the stop is refused rather than
// trusted. reset-failed touches only this unit.
func recoverLinuxInstall(m *linuxServiceManager, config linuxServiceConfig, baseline linuxUnitBaseline, failure *linuxInstallStepError) linuxInstallRecovery {
	r := linuxInstallRecovery{Actions: []string{}, Next: []string{}, RuntimeIdentity: "not_verified", Configuration: "not_restored"}
	if failure.daemonTouched && !baseline.active && r.daemonOnlyStop(m, config.Name) {
		// Stop while the loaded definition is still the one this install wrote.
		r.run(m, "stopped the daemon this install started", "stop", config.Name)
	}
	if linuxEnablementAction(baseline.unitFileState) == linuxEnablementEnable {
		// Only this case enabled anything. Disable through the definition whose
		// enable added the links, before it is replaced or removed.
		r.run(m, "removed the login enablement this install added", "disable", config.Name)
	}
	if r.restoreUnitFile(m, config, baseline) {
		r.Loaded = r.run(m, "reloaded user service definitions", "daemon-reload")
		if !r.Loaded {
			r.Next = append(r.Next, "the previous definition is on disk but systemd has not loaded it; run systemctl --user daemon-reload and check systemctl --user show "+config.Name+" --property=KillMode reports process before restarting it")
		}
	}
	if failure.daemonTouched && baseline.active {
		if r.Configuration == "restored" && r.Loaded {
			r.restartPrevious(m, config)
		} else {
			r.failed("the previous runtime was not restarted because its configuration is not restored and loaded", "nothing was restarted by recovery; restore and load the previous definition first")
		}
	}
	r.checkEnablementRestored(m, config.Name, baseline)
	r.ServiceState = observedServiceState(m, config.Name)
	r.Outcome = linuxRecoveryOutcome(&r, config, baseline)
	return r
}

// daemonOnlyStop reports whether systemd itself says a stop of this unit
// signals only its main process. Anything else -- another mode, or no answer
// -- is recorded and the caller does not signal.
func (r *linuxInstallRecovery) daemonOnlyStop(m *linuxServiceManager, name string) bool {
	mode, err := effectiveKillMode(m, name)
	r.KillMode = mode
	if err != nil {
		r.failed("could not read the effective KillMode of "+name+": "+err.Error(),
			"nothing was stopped or restarted by recovery; inspect systemctl --user cat "+name+" and its drop-ins before acting")
		return false
	}
	if r.KillMode != "process" {
		r.failed("the effective KillMode of "+name+" is "+r.KillMode+", so a stop or restart could signal runners",
			"nothing was stopped or restarted by recovery; review systemctl --user cat "+name+" and its drop-ins")
		return false
	}
	return true
}

func (r *linuxInstallRecovery) restoreUnitFile(m *linuxServiceManager, config linuxServiceConfig, baseline linuxUnitBaseline) bool {
	if !baseline.existed {
		if err := os.Remove(config.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			r.failed("remove the new unit: "+err.Error(), "remove "+config.Path+" and run systemctl --user daemon-reload")
			return false
		}
		r.Configuration = "removed"
		r.Actions = append(r.Actions, "removed the unit this install wrote")
		return true
	}
	err := m.writeUnit(config.Path, string(baseline.unit))
	if err == nil {
		err = m.chmodUnit(config.Path, baseline.mode)
	}
	if err != nil {
		r.PreviousUnit = string(baseline.unit)
		r.failed("restore the previous unit: "+err.Error(),
			fmt.Sprintf("%s may still hold the failed definition; write previous_unit back to it with mode %#o, then run systemctl --user daemon-reload", config.Path, baseline.mode))
		return false
	}
	r.Configuration = "restored"
	r.Actions = append(r.Actions, "restored the previous unit file byte for byte")
	return true
}

func (r *linuxInstallRecovery) restartPrevious(m *linuxServiceManager, config linuxServiceConfig) {
	if !r.daemonOnlyStop(m, config.Name) {
		return
	}
	r.run(m, "cleared the failed state of "+config.Name, "reset-failed", config.Name)
	if !r.run(m, "restarted the unit from the restored definition", "restart", config.Name) {
		r.Next = append(r.Next, "inspect journalctl --user -u "+config.Name)
		return
	}
	healthy := m.healthy(config.Name) == nil
	r.Healthy = &healthy
	if !healthy {
		r.failed("the restarted unit did not answer its health check", "inspect journalctl --user -u "+config.Name)
	}
}

// observedServiceState is systemd's own answer after recovery; is-active
// exits non-zero for every state but active, so its output is what counts.
func observedServiceState(m *linuxServiceManager, name string) string {
	output, _ := m.systemctl("is-active", name)
	switch state := strings.TrimSpace(string(output)); state {
	case "active", "inactive", "failed", "activating", "deactivating", "reloading":
		return state
	}
	return "unknown"
}

func linuxRecoveryOutcome(r *linuxInstallRecovery, config linuxServiceConfig, baseline linuxUnitBaseline) string {
	journal := "inspect journalctl --user -u " + config.Name
	switch {
	case len(r.Errors) > 0:
		r.Next = append(r.Next, "recovery was incomplete; "+journal+", then rerun sessions install from the runtime you intend to keep")
		return "restore_failed"
	case !baseline.existed:
		r.Next = append(r.Next, "no earlier service existed, so nothing was rolled back; the staged runtime remains at "+config.Daemon+" for diagnosis; "+journal+", then rerun sessions install")
		return "not_installed"
	case !r.Loaded:
		r.Next = append(r.Next, "the previous configuration is restored on disk; no daemon-reload was performed; "+journal+" before retrying the update")
		return "restored"
	default:
		r.Next = append(r.Next, "the previous configuration is restored and loaded; "+journal+" before retrying the update")
		return "restored"
	}
}

func (a *app) reportLinuxInstallFailure(config linuxServiceConfig, failure *linuxInstallStepError, recovery linuxInstallRecovery) error {
	message := fmt.Sprintf("install %s failed at %s: %s; recovery %s: %s", config.Name, failure.step, failure.detail, recovery.Outcome, strings.Join(recovery.Next, "; "))
	if !a.wantJSON {
		return fail(2, "%s", message)
	}
	_ = writeJSON(a.stdout, map[string]any{"ok": false, "code": 2, "error": message, "service": config.Name, "unit_path": config.Path,
		"failed_step": failure.step, "attempted_runtime_path": config.Daemon, "recovery": recovery}, true)
	return status(2)
}

// reportLinuxUnitWriteFailure handles a failed write of the candidate unit.
// A writer can fail after the candidate is already in place -- writeDaemonPlist
// renames before its final chmod -- so the file is compared with the baseline
// rather than assumed unchanged. Nothing was loaded, enabled, or started yet,
// so recovery only puts the file back: it never reloads or restarts.
func (a *app) reportLinuxUnitWriteFailure(m *linuxServiceManager, config linuxServiceConfig, baseline linuxUnitBaseline, writeErr error) error {
	if linuxUnitMatchesBaseline(config.Path, baseline) {
		return fail(2, "write %s: %v; verified the previous service definition is unchanged, and nothing was loaded, enabled, started, or restarted", config.Path, writeErr)
	}
	r := linuxInstallRecovery{Actions: []string{}, Next: []string{}, RuntimeIdentity: "not_verified", Configuration: "not_restored"}
	if r.restoreUnitFile(m, config, baseline) {
		r.Actions = append(r.Actions, "no daemon-reload was needed: systemd never loaded the candidate")
	}
	r.checkEnablementRestored(m, config.Name, baseline)
	r.ServiceState = observedServiceState(m, config.Name)
	r.Outcome = linuxRecoveryOutcome(&r, config, baseline)
	return a.reportLinuxInstallFailure(config, &linuxInstallStepError{step: "write", detail: writeErr.Error()}, r)
}

func linuxUnitMatchesBaseline(path string, baseline linuxUnitBaseline) bool {
	current, err := os.ReadFile(path)
	if !baseline.existed {
		return errors.Is(err, fs.ErrNotExist)
	}
	info, statErr := os.Stat(path)
	return err == nil && statErr == nil && bytes.Equal(current, baseline.unit) && info.Mode().Perm() == baseline.mode
}
