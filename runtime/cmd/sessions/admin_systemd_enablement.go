package main

import "strings"

// Login enablement follows systemd's UnitFileState. A persistent enable
// (systemctl enable) writes links that survive reboot; enable --runtime
// writes them under /run, reported as enabled-runtime and lost at reboot.
// disable removes every link to the unit, not only the ones a matching enable
// made, so the two are not symmetric. Install therefore performs a persistent
// enable only from a disabled or absent unit -- the one state recovery can
// restore with disable -- and leaves every other state as it found it.
const (
	linuxEnablementEnable          = "enabled"
	linuxEnablementAlreadyEnabled  = "already_enabled"
	linuxEnablementKeptRuntimeOnly = "kept_runtime_only"
	linuxEnablementLeftAsFound     = "left_as_found"
)

// linuxEnablementAction is what install does to login enablement, decided
// from the state systemd reported before any change. An empty state is a
// unit systemd has no file for yet: a first install.
func linuxEnablementAction(unitFileState string) string {
	switch normalizedUnitFileState(unitFileState) {
	case "", "disabled":
		return linuxEnablementEnable
	case "enabled":
		return linuxEnablementAlreadyEnabled
	case "enabled-runtime":
		return linuxEnablementKeptRuntimeOnly
	}
	return linuxEnablementLeftAsFound
}

func normalizedUnitFileState(state string) string {
	if state = strings.TrimSpace(state); state == "not-found" {
		return ""
	}
	return state
}

// observedUnitFileState is systemd's own answer, or unknown when it gives
// none. An empty value is real: the unit has no file.
func observedUnitFileState(m *linuxServiceManager, name string) string {
	output, err := m.systemctl("show", name, "--property=UnitFileState")
	state, present := systemdProperties(output)["UnitFileState"]
	if err != nil || !present {
		return "unknown"
	}
	return normalizedUnitFileState(state)
}

func linuxEnablementNote(action, state, name string) string {
	switch action {
	case linuxEnablementKeptRuntimeOnly:
		return "Login enablement was runtime-only (enabled-runtime, lost at reboot) and was kept that way; to start at every login run systemctl --user enable " + name + "."
	case linuxEnablementLeftAsFound:
		return "Login enablement was " + state + " and was left as found; review systemctl --user is-enabled " + name + " if it should start at login."
	}
	return ""
}

// checkEnablementRestored compares systemd's enablement after recovery with
// the state found before install. A difference or no answer is reported,
// never described as restored.
func (r *linuxInstallRecovery) checkEnablementRestored(m *linuxServiceManager, name string, baseline linuxUnitBaseline) {
	r.UnitFileState = observedUnitFileState(m, name)
	want := normalizedUnitFileState(baseline.unitFileState)
	switch {
	case r.UnitFileState == "unknown":
		r.failed("login enablement after recovery could not be read", "check systemctl --user is-enabled "+name+"; before install it was "+describeUnitFileState(want))
	case r.UnitFileState != want:
		r.failed("login enablement is "+describeUnitFileState(r.UnitFileState)+", not "+describeUnitFileState(want)+" as found before install",
			"return it with systemctl --user enable or disable "+name+" (add --runtime for runtime-only enablement)")
	}
}

func describeUnitFileState(state string) string {
	if state == "" {
		return "absent (no unit file)"
	}
	return state
}
