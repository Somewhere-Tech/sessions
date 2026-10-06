package main

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// assertNoActivation fails if install loaded, enabled, disabled, or signalled
// the service: none of that may happen once a write has failed.
func (f *fakeUserSystemd) assertNoActivation(t *testing.T) {
	t.Helper()
	for _, call := range f.calls {
		if verb := strings.Fields(call)[0]; slices.Contains([]string{"daemon-reload", "enable", "disable", "start", "restart", "stop", "reset-failed"}, verb) {
			t.Fatalf("install activated or signalled after a failed write: %v", f.calls)
		}
	}
}

// A runtime-only enablement (enable --runtime, lost at reboot) is kept as it
// is: install does not make it persistent, and recovery has nothing to undo.
func TestLinuxRuntimeOnlyEnablementIsNotMadePersistent(t *testing.T) {
	fake := &fakeUserSystemd{active: true, runtimeEnabled: true}
	fixture := newLinuxInstallFixture(t, fake, "sessions", previousSessionsUnit)
	if err := fixture.app.installLinuxService([]string{"--restart-daemon"}); err != nil {
		t.Fatal(err)
	}
	expect(t, fixture.result(t), map[string]any{"enablement": "kept_runtime_only", "unit_file_state": "enabled-runtime", "daemon_action": "restart"})
	if fake.enabled || fake.called("enable sessions.service") {
		t.Fatalf("install turned runtime-only enablement into a persistent one: %v", fake.calls)
	}

	fixture.app.wantJSON = false
	fixture.out.Reset()
	if err := fixture.app.installLinuxService(nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fixture.out.String(), "runtime-only (enabled-runtime, lost at reboot) and was kept that way") {
		t.Fatalf("text output does not explain the kept enablement:\n%s", fixture.out.String())
	}
}

func TestLinuxRecoveryLeavesRuntimeOnlyEnablementAsFound(t *testing.T) {
	fake := &fakeUserSystemd{active: true, runtimeEnabled: true, fail: map[string][]error{"restart": {errors.New("exit status 1"), nil}}}
	fixture := newLinuxInstallFixture(t, fake, "sessions", previousSessionsUnit)
	recovery := fixture.recovery(t, fixture.app.installLinuxService([]string{"--restart-daemon"}))
	expect(t, recovery, map[string]any{"outcome": "restored", "unit_file_state": "enabled-runtime"})
	if fake.enabled || fake.called("disable sessions.service") || !fake.runtimeEnabled {
		t.Fatalf("recovery changed runtime-only enablement: persistent=%v runtime=%v %v", fake.enabled, fake.runtimeEnabled, fake.calls)
	}
}

func TestLinuxOtherEnablementStatesAreLeftAsFound(t *testing.T) {
	for _, state := range []string{"static", "linked"} {
		t.Run(state, func(t *testing.T) {
			fake := &fakeUserSystemd{unitFileOverride: state}
			fixture := newLinuxInstallFixture(t, fake, "sessions", previousSessionsUnit)
			if err := fixture.app.installLinuxService(nil); err != nil {
				t.Fatal(err)
			}
			expect(t, fixture.result(t), map[string]any{"enablement": "left_as_found", "unit_file_state": state})
			if fake.called("enable sessions.service") || fake.called("disable sessions.service") {
				t.Fatalf("install changed %s enablement: %v", state, fake.calls)
			}
		})
	}
}

// An absent unit has an empty UnitFileState; that is a first install, which
// proceeds and enables persistently.
func TestLinuxFirstInstallFromAbsentUnitEnablesPersistently(t *testing.T) {
	fake := &fakeUserSystemd{}
	fixture := newLinuxInstallFixture(t, fake, "sessions", "")
	if err := fixture.app.installLinuxService(nil); err != nil {
		t.Fatal(err)
	}
	expect(t, fixture.result(t), map[string]any{"enablement": "enabled", "unit_file_state": "enabled"})
}

func TestLinuxUnrestoredEnablementIsReported(t *testing.T) {
	for _, test := range []struct {
		name      string
		fake      *fakeUserSystemd
		wantState string
		wantNext  string
	}{
		{name: "disable fails", fake: &fakeUserSystemd{fail: map[string][]error{"start": {errors.New("exit status 1")}, "disable": {errors.New("exit status 1")}}},
			wantState: "enabled", wantNext: "systemctl --user enable or disable sessions.service"},
		{name: "state cannot be read", fake: &fakeUserSystemd{fail: map[string][]error{"start": {errors.New("exit status 1")}}, unitFileErr: errors.New("exit status 1")},
			wantState: "unknown", wantNext: "before install it was disabled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLinuxInstallFixture(t, test.fake, "sessions", previousSessionsUnit)
			recovery := fixture.recovery(t, fixture.app.installLinuxService(nil))
			expect(t, recovery, map[string]any{"outcome": "restore_failed", "configuration": "restored", "unit_file_state": test.wantState})
			nextMentions(t, recovery, test.wantNext)
		})
	}
}

// A writer may fail before or after the candidate reaches the unit path.
// writeDaemonPlist renames before its final chmod, so "failed" does not mean
// "unchanged": the file on disk decides.
func TestLinuxWriteFailureBeforeTheCandidateLandsChangesNothing(t *testing.T) {
	fake := &fakeUserSystemd{active: true, enabled: true}
	fixture := newLinuxInstallFixture(t, fake, "sessions", previousSessionsUnit)
	fixture.service.writeUnit = func(string, string) error { return errors.New("no space left on device") }
	err := fixture.app.installLinuxService([]string{"--restart-daemon"})
	var failure *cliFailure
	if err == nil || !errors.As(err, &failure) || exitCode(err) != 2 ||
		!strings.Contains(failure.message, "verified the previous service definition is unchanged") {
		t.Fatalf("err = %#v", err)
	}
	assertUnitBytes(t, fixture.unit, previousSessionsUnit, 0o640)
	fake.assertNoActivation(t)
}

func TestLinuxWriteFailureAfterTheCandidateLandsRestoresWithoutActivation(t *testing.T) {
	for _, test := range []struct {
		name, previous, outcome, configuration string
	}{
		{name: "previous unit", previous: previousSessionsUnit, outcome: "restored", configuration: "restored"},
		{name: "first install", outcome: "not_installed", configuration: "removed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeUserSystemd{active: test.previous != "", enabled: test.previous != ""}
			fixture := newLinuxInstallFixture(t, fake, "sessions", test.previous)
			writes := 0
			fixture.service.writeUnit = func(path, content string) error {
				if writes++; writes == 1 {
					if err := writeDaemonPlist(path, content); err != nil {
						t.Fatal(err)
					}
					return errors.New("chmod after rename: operation not permitted")
				}
				return writeDaemonPlist(path, content)
			}
			recovery := fixture.recovery(t, fixture.app.installLinuxService([]string{"--restart-daemon"}))
			if fixture.result(t)["failed_step"] != "write" {
				t.Fatalf("result = %v", fixture.result(t))
			}
			expect(t, recovery, map[string]any{"outcome": test.outcome, "configuration": test.configuration, "loaded": false, "healthy": nil})
			if test.previous != "" {
				assertUnitBytes(t, fixture.unit, test.previous, 0o640)
				nextMentions(t, recovery, "restored on disk; no daemon-reload was performed")
				if strings.Contains(fixture.out.String(), "restored and loaded") {
					t.Fatal("write-only recovery claimed systemd loaded the restored definition")
				}
			} else if !linuxUnitMatchesBaseline(fixture.unit, linuxUnitBaseline{}) {
				t.Fatal("the candidate unit from a failed first install was left behind")
			}
			fake.assertNoActivation(t)
		})
	}
}

func TestLinuxWriteFailureWithFailedRestoreGivesTheExactPreviousUnit(t *testing.T) {
	fake := &fakeUserSystemd{active: true, enabled: true}
	fixture := newLinuxInstallFixture(t, fake, "sessions", previousSessionsUnit)
	writes := 0
	fixture.service.writeUnit = func(path, content string) error {
		if writes++; writes == 1 {
			_ = writeDaemonPlist(path, content)
		}
		return errors.New("read-only file system")
	}
	recovery := fixture.recovery(t, fixture.app.installLinuxService([]string{"--restart-daemon"}))
	expect(t, recovery, map[string]any{"outcome": "restore_failed", "configuration": "not_restored", "previous_unit": previousSessionsUnit})
	nextMentions(t, recovery, "write previous_unit back")
	fake.assertNoActivation(t)
}
