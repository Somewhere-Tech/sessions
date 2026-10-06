package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeUserSystemd stands in for systemctl --user. No installer test reaches
// a real service manager or signals a real process. killMode is what systemd
// would report as the loaded unit's effective policy after drop-ins and
// parsing; it is deliberately independent of the unit file's text. killModes,
// when set, answers successive reads in order (the forward install reads the
// new definition, recovery the restored one); "!error" fails that read.
type fakeUserSystemd struct {
	calls       []string
	active      bool
	activeState string
	enabled     bool // persistent links (systemctl enable)
	// runtimeEnabled is /run links (enable --runtime), lost at reboot.
	runtimeEnabled   bool
	unitFileOverride string // another systemd state such as static or linked
	unitFileErr      error  // fails a UnitFileState-only query
	unitPath         string // a unit with no file reports an empty UnitFileState
	observeErr       error
	killMode         string
	killModeErr      error
	killModes        []string
	fail             map[string][]error
	health           []error
}

// unitFileState models systemd: persistent links win over runtime links, and
// a unit with no file has an empty state.
func (f *fakeUserSystemd) unitFileState() string {
	if f.unitFileOverride != "" {
		return f.unitFileOverride
	}
	if f.unitPath != "" {
		if _, err := os.Stat(f.unitPath); err != nil {
			return ""
		}
	}
	switch {
	case f.enabled:
		return "enabled"
	case f.runtimeEnabled:
		return "enabled-runtime"
	}
	return "disabled"
}

func (f *fakeUserSystemd) show(arguments []string) ([]byte, error) {
	if slices.Contains(arguments, "--property=ActiveState,UnitFileState") {
		if f.observeErr != nil {
			return []byte("Failed to connect to bus: No such file or directory"), f.observeErr
		}
		state := f.activeState
		if state == "" {
			state = map[bool]string{true: "active", false: "inactive"}[f.active]
		}
		return []byte("ActiveState=" + state + "\nUnitFileState=" + f.unitFileState() + "\n"), nil
	}
	if slices.Contains(arguments, "--property=UnitFileState") {
		if f.unitFileErr != nil {
			return []byte("Failed to connect to bus"), f.unitFileErr
		}
		return []byte("UnitFileState=" + f.unitFileState() + "\n"), nil
	}
	mode := f.killMode
	if len(f.killModes) > 0 {
		mode, f.killModes = f.killModes[0], f.killModes[1:]
	}
	if mode == "!error" || (mode == "" && f.killModeErr != nil) {
		return []byte("Failed to connect to bus"), errors.New("exit status 1")
	}
	if mode == "" {
		mode = "process"
	}
	return []byte(mode + "\n"), nil
}

func (f *fakeUserSystemd) systemctl(arguments ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(arguments, " "))
	verb := arguments[0]
	if queue := f.fail[verb]; len(queue) > 0 {
		f.fail[verb] = queue[1:]
		if queue[0] != nil {
			if verb == "start" || verb == "restart" {
				f.active = false
			}
			return []byte(verb + " refused by fake"), queue[0]
		}
	}
	switch verb {
	case "is-active":
		if !f.active {
			return []byte("inactive\n"), errors.New("exit status 3")
		}
		return []byte("active\n"), nil
	case "is-enabled":
		if !f.enabled {
			return nil, errors.New("exit status 1")
		}
	case "show":
		return f.show(arguments)
	case "enable", "disable":
		if slices.Contains(arguments, "--runtime") {
			f.runtimeEnabled = verb == "enable"
		} else {
			f.enabled = verb == "enable"
		}
	case "start", "restart", "stop":
		f.active = verb != "stop"
	}
	return nil, nil
}

func (f *fakeUserSystemd) healthy(string) error {
	if len(f.health) == 0 {
		return nil
	}
	err := f.health[0]
	f.health = f.health[1:]
	return err
}

func (f *fakeUserSystemd) called(call string) bool { return slices.Contains(f.calls, call) }

func (f *fakeUserSystemd) count(call string) int {
	n := 0
	for _, made := range f.calls {
		if made == call {
			n++
		}
	}
	return n
}

// assertNoRecoverySignal fails if recovery stopped or restarted the unit. The
// forward install may already have issued one restart; recovery adds none.
func (f *fakeUserSystemd) assertNoRecoverySignal(t *testing.T, name string, forwardRestarts int) {
	t.Helper()
	if f.called("stop "+name) || f.called("reset-failed "+name) || f.count("restart "+name) != forwardRestarts {
		t.Fatalf("recovery signalled the service: %v", f.calls)
	}
}

// previousSessionsUnit stands for an earlier Sessions-written unit.
const previousSessionsUnit = "[Service]\nExecStart=/old/sessionsd\nKillMode=process\n"

type linuxInstallFixture struct {
	app     *app
	out     *bytes.Buffer
	unit    string
	home    string
	service *linuxServiceManager
}

func newLinuxInstallFixture(t *testing.T, fake *fakeUserSystemd, label, previousUnit string) linuxInstallFixture {
	t.Helper()
	home, bin := t.TempDir(), t.TempDir()
	for _, name := range []string{"sessions", "sessionsd", "sessions-runner"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("fixture "+name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"SESSIONS_BINARY", "SESSIONS_RUNNER", "SESSIONS_STATE_DIR", "SESSIONS_LEDGER_PATH"} {
		t.Setenv(key, "")
	}
	t.Setenv("SESSIONS_DAEMON_LABEL", label)
	unit := filepath.Join(home, ".config", "systemd", "user", label+".service")
	fake.unitPath = unit
	if previousUnit != "" {
		if err := os.MkdirAll(filepath.Dir(unit), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(unit, []byte(previousUnit), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	_ = listener.Close()
	out := &bytes.Buffer{}
	service := &linuxServiceManager{
		lookPath: func(string) (string, error) { return "/usr/bin/systemctl", nil }, systemctl: fake.systemctl, healthy: fake.healthy,
		linger: func() ([]byte, error) { return []byte("no\n"), nil }, executable: func() (string, error) { return filepath.Join(bin, "sessions"), nil },
		writeUnit: writeDaemonPlist, chmodUnit: os.Chmod,
	}
	a := &app{home: home, host: "127.0.0.1", port: port, wantJSON: true, stdout: out, now: time.Now, sleep: func(time.Duration) {}, linuxService: service}
	return linuxInstallFixture{app: a, out: out, unit: unit, home: home, service: service}
}

// failRestoreWrite lets the forward install write its unit and fails the
// recovery's write of the previous bytes.
func (f linuxInstallFixture) failRestoreWrite() {
	writes := 0
	f.service.writeUnit = func(path, content string) error {
		if writes++; writes > 1 {
			return errors.New("read-only file system")
		}
		return writeDaemonPlist(path, content)
	}
}

func (f linuxInstallFixture) result(t *testing.T) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal(f.out.Bytes(), &result); err != nil {
		t.Fatalf("install output is not one JSON document: %v\n%s", err, f.out.String())
	}
	return result
}

func (f linuxInstallFixture) recovery(t *testing.T, err error) map[string]any {
	t.Helper()
	if err == nil || exitCode(err) != 2 {
		t.Fatalf("failed install exit = %d (%v), want 2", exitCode(err), err)
	}
	result := f.result(t)
	if result["ok"] != false {
		t.Fatalf("failure result = %v", result)
	}
	for _, claim := range []string{"runners_stopped", "runners_preserved"} {
		if _, claimed := result[claim]; claimed {
			t.Fatalf("install must not assert what systemd policy did to runners: %s", claim)
		}
	}
	recovery := result["recovery"].(map[string]any)
	if recovery["runtime_identity"] != "not_verified" {
		t.Fatalf("recovery claimed a runtime identity it cannot establish: %v", recovery)
	}
	if next, _ := recovery["next"].([]any); len(next) == 0 {
		t.Fatalf("recovery gave no next action: %v", recovery)
	}
	return recovery
}

func expect(t *testing.T, recovery map[string]any, want map[string]any) {
	t.Helper()
	for key, value := range want {
		if recovery[key] != value {
			t.Fatalf("recovery[%s] = %v, want %v; recovery = %v", key, recovery[key], value, recovery)
		}
	}
}

func nextMentions(t *testing.T, recovery map[string]any, text string) {
	t.Helper()
	next, _ := json.Marshal(recovery["next"])
	if !strings.Contains(string(next), text) {
		t.Fatalf("next actions %s do not mention %q", next, text)
	}
}

func assertUnitBytes(t *testing.T, path, want string, mode os.FileMode) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("unit = %q, %v; want the previous bytes %q", got, err, want)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != mode {
		t.Fatalf("unit mode = %v, %v; want %v", info.Mode().Perm(), err, mode)
	}
}

func TestLinuxFirstInstallStartsServiceAndReportsObservedLinger(t *testing.T) {
	fake := &fakeUserSystemd{}
	fixture := newLinuxInstallFixture(t, fake, "sessions", "")
	if err := fixture.app.installLinuxService(nil); err != nil {
		t.Fatal(err)
	}
	result := fixture.result(t)
	if result["ok"] != true || result["daemon_action"] != "start" || result["linger"] != "disabled" {
		t.Fatalf("result = %v", result)
	}
	if _, promised := result["runners_survive_logout"]; promised {
		t.Fatal("install must report the observed linger setting, not a survival promise")
	}
	for _, call := range []string{"daemon-reload", "enable sessions.service", "start sessions.service"} {
		if !fake.called(call) {
			t.Fatalf("missing %q in %v", call, fake.calls)
		}
	}
	unit, err := os.ReadFile(fixture.unit)
	if err != nil || !strings.Contains(string(unit), filepath.Join(fixture.home, ".local", "share", "sessions", "runtime")) {
		t.Fatalf("unit does not run the staged runtime: %q %v", unit, err)
	}
}

func TestLinuxRestartFailureRestoresPreviousUnitAndRuntime(t *testing.T) {
	fake := &fakeUserSystemd{active: true, enabled: true, fail: map[string][]error{"restart": {errors.New("exit status 1"), nil}}}
	fixture := newLinuxInstallFixture(t, fake, "sessions-beta", previousSessionsUnit)
	recovery := fixture.recovery(t, fixture.app.installLinuxService([]string{"--restart-daemon"}))
	if fixture.result(t)["failed_step"] != "restart" {
		t.Fatalf("result = %v", fixture.result(t))
	}
	expect(t, recovery, map[string]any{"outcome": "restored", "configuration": "restored", "loaded": true,
		"kill_mode": "process", "healthy": true, "service_state": "active"})
	assertUnitBytes(t, fixture.unit, previousSessionsUnit, 0o640)
	if fake.called("stop sessions-beta.service") || fake.called("disable sessions-beta.service") {
		t.Fatalf("recovery stopped or disabled a service that was running and enabled: %v", fake.calls)
	}
	reset := slices.Index(fake.calls, "reset-failed sessions-beta.service")
	lastRestart := -1
	for index, call := range fake.calls {
		if call == "restart sessions-beta.service" {
			lastRestart = index
		}
	}
	if reset < slices.Index(fake.calls, "show sessions-beta.service --property=KillMode --value") || fake.count("restart sessions-beta.service") != 2 ||
		lastRestart < reset {
		t.Fatalf("previous runtime was not restarted after a policy read and unit-scoped reset: %v", fake.calls)
	}
}

func TestLinuxNewDaemonHealthFailureRestoresPreviousRuntime(t *testing.T) {
	fake := &fakeUserSystemd{active: true, enabled: true, health: []error{errors.New("not healthy within 15 seconds"), nil}}
	fixture := newLinuxInstallFixture(t, fake, "sessions", previousSessionsUnit)
	recovery := fixture.recovery(t, fixture.app.installLinuxService([]string{"--restart-daemon"}))
	if fixture.result(t)["failed_step"] != "health" {
		t.Fatalf("result = %v", fixture.result(t))
	}
	expect(t, recovery, map[string]any{"outcome": "restored", "configuration": "restored", "healthy": true, "service_state": "active"})
	assertUnitBytes(t, fixture.unit, previousSessionsUnit, 0o640)
}

func TestLinuxFailedRecoveryRestartIsReportedRatherThanClaimed(t *testing.T) {
	fake := &fakeUserSystemd{active: true, enabled: true, fail: map[string][]error{"restart": {errors.New("exit status 1"), errors.New("exit status 1")}}}
	fixture := newLinuxInstallFixture(t, fake, "sessions", previousSessionsUnit)
	recovery := fixture.recovery(t, fixture.app.installLinuxService([]string{"--restart-daemon"}))
	expect(t, recovery, map[string]any{"outcome": "restore_failed", "configuration": "restored", "loaded": true, "healthy": nil, "service_state": "inactive"})
	assertUnitBytes(t, fixture.unit, previousSessionsUnit, 0o640)
	nextMentions(t, recovery, "journalctl --user -u sessions.service")
}

func TestLinuxRestoreWriteOrChmodFailureNeverRestartsTheNewDefinition(t *testing.T) {
	for _, fault := range []string{"write", "chmod"} {
		t.Run(fault, func(t *testing.T) {
			fake := &fakeUserSystemd{active: true, enabled: true, fail: map[string][]error{"restart": {errors.New("exit status 1")}}}
			fixture := newLinuxInstallFixture(t, fake, "sessions", previousSessionsUnit)
			if fault == "write" {
				fixture.failRestoreWrite()
			} else {
				fixture.service.chmodUnit = func(string, fs.FileMode) error { return errors.New("operation not permitted") }
			}
			recovery := fixture.recovery(t, fixture.app.installLinuxService([]string{"--restart-daemon"}))
			expect(t, recovery, map[string]any{"outcome": "restore_failed", "configuration": "not_restored", "loaded": false,
				"healthy": nil, "previous_unit": previousSessionsUnit})
			fake.assertNoRecoverySignal(t, "sessions.service", 1)
			if fake.count("daemon-reload") != 1 {
				t.Fatalf("recovery reloaded a definition it had not restored: %v", fake.calls)
			}
			nextMentions(t, recovery, "write previous_unit back")
		})
	}
}

func TestLinuxRecoveryReloadFailureNeverRestartsTheNewDefinition(t *testing.T) {
	fake := &fakeUserSystemd{active: true, enabled: true, fail: map[string][]error{
		"restart": {errors.New("exit status 1")}, "daemon-reload": {nil, errors.New("exit status 1")}}}
	fixture := newLinuxInstallFixture(t, fake, "sessions", previousSessionsUnit)
	recovery := fixture.recovery(t, fixture.app.installLinuxService([]string{"--restart-daemon"}))
	expect(t, recovery, map[string]any{"outcome": "restore_failed", "configuration": "restored", "loaded": false, "healthy": nil})
	fake.assertNoRecoverySignal(t, "sessions.service", 1)
	assertUnitBytes(t, fixture.unit, previousSessionsUnit, 0o640)
	nextMentions(t, recovery, "systemctl --user daemon-reload")
}

// The unit text is not the policy systemd applies: a drop-in, a duplicate
// setting, or a KillMode line in the wrong section all change it. Recovery
// follows only systemd's own effective answer.
func TestLinuxRecoverySignalsOnlyWhenEffectiveKillModeIsProcess(t *testing.T) {
	for _, test := range []struct {
		name, unit, effective string
		showErr               error
		restarted             bool
	}{
		{name: "drop-in overrides generated unit", unit: previousSessionsUnit, effective: "control-group"},
		{name: "duplicate later setting wins", unit: previousSessionsUnit + "KillMode=mixed\n", effective: "mixed"},
		{name: "KillMode in the wrong section", unit: "[Unit]\nKillMode=process\n[Service]\nExecStart=/old/sessionsd\n", effective: "control-group"},
		{name: "policy cannot be read", unit: previousSessionsUnit, showErr: errors.New("exit status 1")},
		{name: "effective policy without the literal line", unit: "[Service]\nExecStart=/old/sessionsd\n", effective: "process", restarted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			recoveryRead := test.effective
			if test.showErr != nil {
				recoveryRead = "!error"
			}
			fake := &fakeUserSystemd{active: true, enabled: true, killModes: []string{"process", recoveryRead},
				health: []error{errors.New("not healthy within 15 seconds")}}
			fixture := newLinuxInstallFixture(t, fake, "sessions", test.unit)
			recovery := fixture.recovery(t, fixture.app.installLinuxService([]string{"--restart-daemon"}))
			assertUnitBytes(t, fixture.unit, test.unit, 0o640)
			if test.restarted {
				expect(t, recovery, map[string]any{"outcome": "restored", "kill_mode": "process", "healthy": true})
				return
			}
			fake.assertNoRecoverySignal(t, "sessions.service", 1)
			want := test.effective
			if test.showErr != nil {
				want = "unknown"
			}
			expect(t, recovery, map[string]any{"outcome": "restore_failed", "configuration": "restored", "loaded": true, "kill_mode": want, "healthy": nil})
			nextMentions(t, recovery, "nothing was stopped or restarted by recovery")
		})
	}
}

func TestLinuxFailedFirstInstallLeavesNoServiceBehind(t *testing.T) {
	fake := &fakeUserSystemd{health: []error{errors.New("not healthy within 15 seconds")}}
	fixture := newLinuxInstallFixture(t, fake, "sessions", "")
	recovery := fixture.recovery(t, fixture.app.installLinuxService(nil))
	expect(t, recovery, map[string]any{"outcome": "not_installed", "configuration": "removed", "loaded": true, "service_state": "inactive", "healthy": nil})
	if _, err := os.Stat(fixture.unit); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new unit was left behind: %v", err)
	}
	show, stop, disable := slices.Index(fake.calls, "show sessions.service --property=KillMode --value"),
		slices.Index(fake.calls, "stop sessions.service"), slices.Index(fake.calls, "disable sessions.service")
	if show < 0 || stop < show || disable < stop {
		t.Fatalf("first-install cleanup order = %v", fake.calls)
	}
	staged, err := os.ReadDir(filepath.Join(fixture.home, ".local", "share", "sessions", "runtime"))
	if err != nil || len(staged) != 1 {
		t.Fatalf("staged runtime should remain for diagnosis: %v %v", staged, err)
	}
}

func TestLinuxFailedFirstInstallDoesNotStopUnderWiderPolicy(t *testing.T) {
	fake := &fakeUserSystemd{killMode: "control-group", health: []error{errors.New("not healthy within 15 seconds")}}
	fixture := newLinuxInstallFixture(t, fake, "sessions", "")
	recovery := fixture.recovery(t, fixture.app.installLinuxService(nil))
	expect(t, recovery, map[string]any{"outcome": "restore_failed", "kill_mode": "control-group", "service_state": "active"})
	fake.assertNoRecoverySignal(t, "sessions.service", 0)
}

func TestLinuxStartFailureRestoresStoppedDisabledService(t *testing.T) {
	fake := &fakeUserSystemd{fail: map[string][]error{"start": {errors.New("exit status 1")}}}
	fixture := newLinuxInstallFixture(t, fake, "sessions", previousSessionsUnit)
	recovery := fixture.recovery(t, fixture.app.installLinuxService(nil))
	expect(t, recovery, map[string]any{"outcome": "restored", "configuration": "restored", "loaded": true, "service_state": "inactive", "healthy": nil})
	if fake.enabled {
		t.Fatalf("recovery left an enablement the baseline did not have: %v", fake.calls)
	}
	assertUnitBytes(t, fixture.unit, previousSessionsUnit, 0o640)
}

func TestLinuxReloadFailureLeavesRunningDaemonUntouched(t *testing.T) {
	fake := &fakeUserSystemd{active: true, enabled: true, fail: map[string][]error{"daemon-reload": {errors.New("exit status 1")}}}
	fixture := newLinuxInstallFixture(t, fake, "sessions", previousSessionsUnit)
	recovery := fixture.recovery(t, fixture.app.installLinuxService(nil))
	expect(t, recovery, map[string]any{"outcome": "restored", "configuration": "restored", "loaded": true, "service_state": "active", "healthy": nil})
	for _, call := range fake.calls {
		if verb := strings.Fields(call)[0]; slices.Contains([]string{"stop", "start", "restart", "reset-failed", "disable"}, verb) {
			t.Fatalf("recovery touched a running daemon that this install never changed: %v", fake.calls)
		}
	}
	assertUnitBytes(t, fixture.unit, previousSessionsUnit, 0o640)
}
