package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func assertNoRunnerLifetimeClaim(t *testing.T, result map[string]any) {
	t.Helper()
	for _, claim := range []string{"runners_preserved", "runners_stopped", "runners_survive_logout"} {
		if _, claimed := result[claim]; claimed {
			t.Fatalf("install claimed %s, which depends on policy it does not control: %v", claim, result)
		}
	}
}

// A forward restart stops the running daemon under the loaded unit's
// effective KillMode, so it is issued only when systemd reports process. Any
// other answer, or none, leaves the running daemon exactly as it was.
func TestLinuxForwardRestartRequiresEffectiveKillModeProcess(t *testing.T) {
	for _, test := range []struct {
		name, killMode string
		restarted      bool
	}{
		{name: "effective process", killMode: "process", restarted: true},
		{name: "drop-in sets control-group", killMode: "control-group"},
		{name: "drop-in sets mixed", killMode: "mixed"},
		{name: "policy read fails", killMode: "!error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeUserSystemd{active: true, enabled: true, killModes: []string{test.killMode}}
			fixture := newLinuxInstallFixture(t, fake, "sessions", previousSessionsUnit)
			err := fixture.app.installLinuxService([]string{"--restart-daemon"})
			if test.restarted {
				if err != nil {
					t.Fatal(err)
				}
				result := fixture.result(t)
				expect(t, result, map[string]any{"daemon_action": "restart", "kill_mode": "process", "healthy": true,
					"service_state": "active", "configuration": "installed", "loaded": true, "runtime_identity": "not_verified"})
				assertNoRunnerLifetimeClaim(t, result)
				return
			}
			recovery := fixture.recovery(t, err)
			if fixture.result(t)["failed_step"] != "kill_mode" || !strings.Contains(fixture.result(t)["error"].(string), "was not restarted") {
				t.Fatalf("result = %v", fixture.result(t))
			}
			// The running daemon keeps working; only the staged definition is undone.
			fake.assertNoRecoverySignal(t, "sessions.service", 0)
			if fake.called("start sessions.service") || fake.called("disable sessions.service") {
				t.Fatalf("a refused restart touched the running service: %v", fake.calls)
			}
			expect(t, recovery, map[string]any{"outcome": "restored", "configuration": "restored", "loaded": true,
				"service_state": "active", "healthy": nil})
			assertUnitBytes(t, fixture.unit, previousSessionsUnit, 0o640)
		})
	}
}

// A first start stops no earlier runtime, so it is not refused under another
// policy; the policy is reported, with what it would mean for a later stop.
func TestLinuxFirstStartReportsPolicyWithoutRefusing(t *testing.T) {
	for _, killMode := range []string{"control-group", "!error"} {
		t.Run(killMode, func(t *testing.T) {
			fake := &fakeUserSystemd{killModes: []string{killMode}}
			fixture := newLinuxInstallFixture(t, fake, "sessions", "")
			if err := fixture.app.installLinuxService(nil); err != nil {
				t.Fatal(err)
			}
			want := killMode
			if killMode == "!error" {
				want = "unknown"
			}
			result := fixture.result(t)
			expect(t, result, map[string]any{"daemon_action": "start", "kill_mode": want, "healthy": true, "service_state": "active"})
			assertNoRunnerLifetimeClaim(t, result)
		})
	}
}

func TestLinuxReinstallPreservesRunningDaemonAndReportsPolicy(t *testing.T) {
	fake := &fakeUserSystemd{active: true, enabled: true, killMode: "mixed"}
	fixture := newLinuxInstallFixture(t, fake, "sessions", previousSessionsUnit)
	if err := fixture.app.installLinuxService(nil); err != nil {
		t.Fatal(err)
	}
	result := fixture.result(t)
	expect(t, result, map[string]any{"daemon_action": "preserved", "kill_mode": "mixed", "healthy": nil,
		"service_state": "active", "restart_required": true})
	for _, call := range fake.calls {
		if verb := strings.Fields(call)[0]; verb == "start" || verb == "restart" || verb == "stop" {
			t.Fatalf("a preserving reinstall signalled the service: %v", fake.calls)
		}
	}
}

// The baseline decides between start and restart and what recovery restores.
// Ordinary inactive, failed, and disabled states are facts; an unanswered
// query is not, and must not be read as "no daemon is running".
func TestLinuxBaselineDistinguishesStatesFromFailedObservation(t *testing.T) {
	for _, test := range []struct {
		name, state    string
		enabled        bool
		observeErr     error
		wantActive, ok bool
	}{
		{name: "inactive and disabled", state: "inactive", ok: true},
		{name: "failed counts as not running", state: "failed", ok: true},
		{name: "activating may be running", state: "activating", wantActive: true, ok: true},
		{name: "active and enabled", state: "active", enabled: true, wantActive: true, ok: true},
		{name: "service manager does not answer", observeErr: errors.New("exit status 1")},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeUserSystemd{activeState: test.state, enabled: test.enabled, observeErr: test.observeErr}
			fixture := newLinuxInstallFixture(t, fake, "sessions", previousSessionsUnit)
			config, err := fixture.app.linuxServiceConfig(fixture.service)
			if err != nil {
				t.Fatal(err)
			}
			baseline, err := readLinuxUnitBaseline(fixture.service, config)
			if !test.ok {
				if err == nil || exitCode(err) != 2 || !strings.Contains(err.Error(), "the service definition was not changed") {
					t.Fatalf("failed observation = %v, want an instructional refusal", err)
				}
				return
			}
			wantUnitFile := map[bool]string{true: "enabled", false: "disabled"}[test.enabled]
			if err != nil || baseline.active != test.wantActive || baseline.unitFileState != wantUnitFile {
				t.Fatalf("baseline = %+v, %v", baseline, err)
			}
		})
	}
}

func TestLinuxInstallChangesNothingWhenStateCannotBeObserved(t *testing.T) {
	fake := &fakeUserSystemd{observeErr: errors.New("exit status 1")}
	fixture := newLinuxInstallFixture(t, fake, "sessions", previousSessionsUnit)
	err := fixture.app.installLinuxService([]string{"--restart-daemon"})
	var failure *cliFailure
	if exitCode(err) != 2 || !errors.As(err, &failure) || !strings.Contains(failure.message, "could not observe whether sessions.service is running") {
		t.Fatalf("err = %#v; want an instructional refusal before any change", err)
	}
	assertUnitBytes(t, fixture.unit, previousSessionsUnit, 0o640)
	for _, call := range fake.calls {
		if verb := strings.Fields(call)[0]; verb != "show-environment" && verb != "show" {
			t.Fatalf("install acted without knowing the service state: %v", fake.calls)
		}
	}
}

// Text output carries the same facts as --json, on success and on failure.
func TestLinuxInstallTextAndJSONReportTheSameFacts(t *testing.T) {
	run := func(wantJSON bool, killMode string) (*linuxInstallFixture, error) {
		fake := &fakeUserSystemd{active: true, enabled: true, killModes: []string{killMode}}
		fixture := newLinuxInstallFixture(t, fake, "sessions", previousSessionsUnit)
		fixture.app.wantJSON = wantJSON
		return &fixture, fixture.app.installLinuxService([]string{"--restart-daemon"})
	}
	jsonFixture, err := run(true, "process")
	if err != nil {
		t.Fatal(err)
	}
	result := jsonFixture.result(t)
	textFixture, err := run(false, "process")
	if err != nil {
		t.Fatal(err)
	}
	text := textFixture.out.String()
	for _, fact := range []string{"daemon action: " + result["daemon_action"].(string), "service state: " + result["service_state"].(string),
		"Effective KillMode: " + result["kill_mode"].(string), "answered its health check", "runtime identity not verified"} {
		if !strings.Contains(text, fact) {
			t.Fatalf("text output lacks %q:\n%s", fact, text)
		}
	}
	if strings.Contains(text, "Runners were not stopped") {
		t.Fatalf("text output promises runner lifetime:\n%s", text)
	}

	jsonFailure, jsonErr := run(true, "control-group")
	failure := jsonFailure.result(t)
	_, textErr := run(false, "control-group")
	if exitCode(jsonErr) != exitCode(textErr) || textErr == nil || textErr.Error() != failure["error"] {
		t.Fatalf("failure parity: json %v / %v, text %v", failure["error"], exitCode(jsonErr), textErr)
	}
	for _, fact := range []string{"failed at kill_mode", "recovery restored"} {
		if !strings.Contains(textErr.Error(), fact) {
			t.Fatalf("text failure lacks %q: %v", fact, textErr)
		}
	}
	if bytes.Contains(jsonFailure.out.Bytes(), []byte("runners_preserved")) {
		t.Fatal("failure JSON claims runner lifetime")
	}
}
