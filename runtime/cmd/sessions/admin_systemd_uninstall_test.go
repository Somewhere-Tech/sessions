package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// shippedSessionsUnit is the text install has generated since Linux install
// was added (d02c53e) for HOME=/home/u with the staged runtime testDigest.
// Uninstall must keep accepting it even if the template changes.
const shippedSessionsUnit = `[Unit]
Description=Sessions durable conversation daemon
After=network.target

[Service]
Type=simple
ExecStart="/home/u/.local/share/sessions/runtime/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef/sessionsd"
Environment="HOME=/home/u"
Environment="PATH=/usr/local/bin:/usr/bin:/bin"
Environment="SESSIONS_HOST=127.0.0.1"
Environment="SESSIONS_PORT=8787"
Environment="SESSIONS_RUNNER=/home/u/.local/share/sessions/runtime/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef/sessions-runner"
Restart=on-failure
RestartSec=2
# Runners own their work independently. A daemon stop must signal only the daemon.
KillMode=process
TimeoutStopSec=10
UMask=0077

[Install]
WantedBy=default.target
`

const testDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// sessionsUnitFor is the unit install generates for home with a staged
// runtime named testDigest, plus any optional environment it passed through.
func sessionsUnitFor(home string, optional ...plistEnvironment) string {
	staged := filepath.Join(linuxRuntimeRoot(home), testDigest)
	environment := []plistEnvironment{{Key: "HOME", Value: home}, {Key: "PATH", Value: "/usr/local/bin:/usr/bin:/bin"},
		{Key: "SESSIONS_HOST", Value: "127.0.0.1"}, {Key: "SESSIONS_PORT", Value: "8787"}, {Key: "SESSIONS_RUNNER", Value: filepath.Join(staged, "sessions-runner")}}
	return linuxServiceUnit(linuxServiceConfig{Daemon: filepath.Join(staged, "sessionsd"), Env: append(environment, optional...)})
}

// newSessionsUnitFixture is a fixture whose unit path holds the definition
// install generates for the fixture's own home.
func newSessionsUnitFixture(t *testing.T, fake *fakeUserSystemd) (linuxInstallFixture, string) {
	t.Helper()
	fixture := newLinuxInstallFixture(t, fake, "sessions", "")
	unit := sessionsUnitFor(fixture.home)
	if err := os.MkdirAll(filepath.Dir(fixture.unit), 0o700); err != nil || os.WriteFile(fixture.unit, []byte(unit), 0o640) != nil {
		t.Fatal("unit fixture", err)
	}
	return fixture, unit
}

// unitVariants each change one thing about the generated Sessions unit for
// /home/u; uninstall must act on none of them.
func unitVariants() map[string]string {
	staged := "/home/u/.local/share/sessions/runtime/" + testDigest
	return map[string]string{
		"appended setting":         shippedSessionsUnit + "Nice=5\n",
		"edited KillMode":          strings.Replace(shippedSessionsUnit, "KillMode=process", "KillMode=mixed", 1),
		"second ExecStart":         strings.Replace(shippedSessionsUnit, "Restart=", "ExecStart=\"/x\"\nRestart=", 1),
		"unescaped specifier":      strings.Replace(shippedSessionsUnit, "HOME=/home/u\"", "HOME=%h\"", 1),
		"unquoted ExecStart":       strings.Replace(shippedSessionsUnit, `ExecStart="`+staged+`/sessionsd"`, "ExecStart="+staged+"/sessionsd", 1),
		"unrelated executable":     strings.Replace(shippedSessionsUnit, `ExecStart="`+staged+`/sessionsd"`, `ExecStart="/usr/bin/unrelated-worker"`, 1),
		"another home's runtime":   sessionsUnitFor("/home/other"),
		"not content-addressed":    strings.ReplaceAll(shippedSessionsUnit, testDigest, "manual"),
		"outside the managed root": strings.ReplaceAll(shippedSessionsUnit, "/home/u/.local/share/sessions/runtime/", "/opt/elsewhere/"),
		"runner outside runtime":   strings.Replace(shippedSessionsUnit, "SESSIONS_RUNNER="+staged+"/sessions-runner", "SESSIONS_RUNNER=/usr/bin/other-runner", 1),
		"unknown environment":      sessionsUnitFor("/home/u", plistEnvironment{Key: "LD_PRELOAD", Value: "/tmp/x.so"}),
		"optional keys reordered":  sessionsUnitFor("/home/u", plistEnvironment{Key: "SHELL", Value: "/bin/sh"}, plistEnvironment{Key: "SESSIONS_STATE_DIR", Value: "/s"}),
		"optional key repeated":    sessionsUnitFor("/home/u", plistEnvironment{Key: "SHELL", Value: "/bin/sh"}, plistEnvironment{Key: "SHELL", Value: "/bin/zsh"}),
		"HOME is another home":     strings.Replace(shippedSessionsUnit, "HOME=/home/u\"", "HOME=/home/other\"", 1),
		"another program's unit":   previousSessionsUnit,
		"empty":                    "",
	}
}

func TestLinuxUninstallRecognisesEveryShippedUnitTemplate(t *testing.T) {
	if got := sessionsUnitFor("/home/u"); got != shippedSessionsUnit {
		t.Fatalf("the install template no longer produces the shipped unit text:\n%s", got)
	}
	awkwardHome := `/tmp/a $HOME %n "q" \ dir`
	awkward := sessionsUnitFor(awkwardHome, plistEnvironment{Key: "SESSIONS_STATE_DIR", Value: "/s/%h:$x\nline\r"}, plistEnvironment{Key: "SHELL", Value: `/bin/"z"\`})
	if !eligibleLinuxUnit([]byte(shippedSessionsUnit), "/home/u") || !eligibleLinuxUnit([]byte(awkward), awkwardHome) {
		t.Fatal("uninstall does not accept a unit install generates")
	}
	for name, unit := range unitVariants() {
		if eligibleLinuxUnit([]byte(unit), "/home/u") {
			t.Fatalf("%s: uninstall accepted a unit that is not the generated Sessions integration:\n%s", name, unit)
		}
	}
}

// assertNoProcessAction fails if uninstall asked systemd to act on a process
// or on any unit but its own, or to reach past persistent enablement.
func assertNoProcessAction(t *testing.T, fake *fakeUserSystemd, from int) {
	t.Helper()
	for _, call := range fake.calls[from:] {
		verb := strings.Fields(call)[0]
		if slices.Contains([]string{"stop", "start", "restart", "try-restart", "reload-or-restart", "kill", "reset-failed", "mask", "unmask", "isolate"}, verb) ||
			strings.Contains(call, "--now") || strings.Contains(call, "--runtime") {
			t.Fatalf("uninstall acted on a process or runtime state: %v", fake.calls)
		}
	}
}

// seedSessionsData writes stand-ins for state, history, and staged runtime
// bytes and returns a check that every one is byte-identical afterwards.
func seedSessionsData(t *testing.T, home string) func() {
	t.Helper()
	files := map[string]string{}
	for _, relative := range []string{".local/state/sessions/runners/r1.json", ".local/state/sessions/ledger/lanes.sqlite3",
		".local/share/sessions/runtime/0123abcd/sessionsd", ".claude/projects/p/conversation.jsonl"} {
		path := filepath.Join(home, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("keep "+relative), 0o600); err != nil {
			t.Fatal(err)
		}
		files[path] = "keep " + relative
	}
	return func() {
		t.Helper()
		for path, want := range files {
			if got, err := os.ReadFile(path); err != nil || string(got) != want {
				t.Fatalf("uninstall changed user data %s: %q %v", path, got, err)
			}
		}
	}
}

func (f linuxInstallFixture) uninstall(t *testing.T, wantExit int) map[string]any {
	t.Helper()
	f.out.Reset()
	err := f.app.uninstallLinuxService(nil)
	if (wantExit == 0) != (err == nil) || (err != nil && exitCode(err) != wantExit) {
		t.Fatalf("uninstall = %v (exit %d), want exit %d\n%s", err, exitCode(err), wantExit, f.out.String())
	}
	result := f.result(t)
	assertNoRunnerLifetimeClaim(t, result)
	expect(t, result, map[string]any{"ok": wantExit == 0, "daemon_action": "none", "daemon_stopped": false, "state_preserved": true, "unit_path": f.unit})
	if next, _ := result["next"].([]any); wantExit != 0 && len(next) == 0 {
		t.Fatalf("a failed uninstall gave no next action: %v", result)
	}
	return result
}

func callsSince(fake *fakeUserSystemd, from int) []string { return slices.Clone(fake.calls[from:]) }

func TestLinuxInstallThenRepeatedUninstallRemovesOnlyItsIntegration(t *testing.T) {
	fake := &fakeUserSystemd{}
	fixture := newLinuxInstallFixture(t, fake, "sessions-beta", "")
	if err := fixture.app.installLinuxService(nil); err != nil {
		t.Fatal(err)
	}
	unchanged := seedSessionsData(t, fixture.home)
	from := len(fake.calls)
	result := fixture.uninstall(t, 0)
	expect(t, result, map[string]any{"unit_file": "removed", "removed": fixture.unit, "service_state": "active", "unit_file_state": ""})
	if fake.enabled {
		t.Fatalf("persistent enablement survived: %v", fake.calls)
	}
	assertNoProcessAction(t, fake, from)
	if calls := callsSince(fake, from); !slices.Equal(calls, []string{"show-environment",
		"show sessions-beta.service --property=LoadState,UnitFileState,FragmentPath,DropInPaths", "disable sessions-beta.service",
		"daemon-reload", "is-active sessions-beta.service", "show sessions-beta.service --property=UnitFileState"}) {
		t.Fatalf("uninstall calls = %q", calls)
	}
	if _, err := os.Lstat(fixture.unit); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unit file remains: %v", err)
	}
	staged, err := os.ReadDir(filepath.Join(fixture.home, ".local", "share", "sessions", "runtime"))
	if err != nil || len(staged) != 2 {
		t.Fatalf("staged runtimes changed: %v %v", staged, err)
	}
	from = len(fake.calls)
	again := fixture.uninstall(t, 0)
	expect(t, again, map[string]any{"unit_file": "absent", "removed": "", "service_state": "active"})
	for _, call := range callsSince(fake, from) {
		if call == "daemon-reload" || strings.HasPrefix(call, "disable") {
			t.Fatalf("a repeated uninstall changed systemd again: %v", fake.calls[from:])
		}
	}
	assertNoProcessAction(t, fake, from)
	unchanged()
}

func TestLinuxUninstallLeavesWhatIsNotTheGeneratedIntegration(t *testing.T) {
	for _, test := range []struct {
		name     string
		previous func(home string) string
		fake     *fakeUserSystemd
		mask     bool
		exit     int
		kept     string
	}{
		{name: "never installed", fake: &fakeUserSystemd{}},
		{name: "same name in another directory", fake: &fakeUserSystemd{enabled: true, foreignFragment: "/usr/lib/systemd/user/sessions.service"},
			kept: "/usr/lib/systemd/user/sessions.service"},
		{name: "mask", fake: &fakeUserSystemd{}, mask: true, kept: "never writes"},
		{name: "another program's unit", previous: func(string) string { return previousSessionsUnit }, fake: &fakeUserSystemd{enabled: true, active: true}, exit: 2},
		{name: "edited Sessions unit", previous: func(home string) string { return sessionsUnitFor(home) + "Nice=5\n" },
			fake: &fakeUserSystemd{enabled: true, active: true}, exit: 2},
		// The coordinator's counterexample: the generated shape around an
		// unrelated executable.
		{name: "unrelated executable in the generated shape", previous: func(home string) string {
			staged := filepath.Join(linuxRuntimeRoot(home), testDigest)
			return strings.Replace(sessionsUnitFor(home), `ExecStart="`+staged+`/sessionsd"`, `ExecStart="/usr/bin/unrelated-worker"`, 1)
		}, fake: &fakeUserSystemd{enabled: true, active: true}, exit: 2},
		{name: "another home's generated unit", previous: func(string) string { return shippedSessionsUnit },
			fake: &fakeUserSystemd{enabled: true, active: true}, exit: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLinuxInstallFixture(t, test.fake, "sessions", "")
			previous := ""
			if test.previous != nil {
				previous = test.previous(fixture.home)
				if err := os.MkdirAll(filepath.Dir(fixture.unit), 0o700); err != nil || os.WriteFile(fixture.unit, []byte(previous), 0o640) != nil {
					t.Fatal("unit fixture", err)
				}
			}
			if test.mask {
				if err := os.MkdirAll(filepath.Dir(fixture.unit), 0o700); err != nil || os.Symlink("/dev/null", fixture.unit) != nil {
					t.Fatal("mask fixture", err)
				}
			}
			unchanged := seedSessionsData(t, fixture.home)
			result := fixture.uninstall(t, test.exit)
			if test.fake.called("daemon-reload") || slices.ContainsFunc(test.fake.calls, func(call string) bool { return strings.HasPrefix(call, "disable") }) {
				t.Fatalf("uninstall changed a unit it did not write: %v", test.fake.calls)
			}
			if result["removed"] != "" || (previous != "" && !test.fake.enabled) {
				t.Fatalf("result = %v, enabled = %v", result, test.fake.enabled)
			}
			if previous != "" {
				assertUnitBytes(t, fixture.unit, previous, 0o640)
				nextMentions(t, result, shellCommand("rm", "--", fixture.unit))
			}
			if target, err := os.Readlink(fixture.unit); test.mask && (err != nil || target != "/dev/null") {
				t.Fatalf("mask was not left as found: %q %v", target, err)
			}
			if kept, _ := result["kept"].([]any); test.kept != "" && !strings.Contains(strings.Join(anyStrings(kept), "\n"), test.kept) {
				t.Fatalf("kept %v does not mention %q", kept, test.kept)
			}
			assertNoProcessAction(t, test.fake, 0)
			unchanged()
		})
	}
}

func anyStrings(values []any) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, value.(string))
	}
	return out
}

func TestLinuxUninstallScopesEnablementToPersistentLinks(t *testing.T) {
	for _, test := range []struct {
		name string
		fake *fakeUserSystemd
		kept string
	}{
		{name: "enabled and running", fake: &fakeUserSystemd{held: true, enabled: true, active: true}},
		{name: "disabled and stopped", fake: &fakeUserSystemd{held: true}},
		{name: "runtime-only", fake: &fakeUserSystemd{held: true, runtimeEnabled: true, active: true}, kept: "runtime-only enablement"},
		{name: "drop-in override", fake: &fakeUserSystemd{held: true, enabled: true, dropIns: "/home/u/.config/systemd/user/sessions.service.d/override.conf"},
			kept: "override.conf"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, _ := newSessionsUnitFixture(t, test.fake)
			wasActive, wasRuntime := test.fake.active, test.fake.runtimeEnabled
			result := fixture.uninstall(t, 0)
			expect(t, result, map[string]any{"unit_file": "removed", "removed": fixture.unit})
			if test.fake.enabled || test.fake.runtimeEnabled != wasRuntime || test.fake.active != wasActive || test.fake.held {
				t.Fatalf("enabled=%v runtime=%v active=%v held=%v after %v", test.fake.enabled, test.fake.runtimeEnabled, test.fake.active, test.fake.held, test.fake.calls)
			}
			if kept, _ := result["kept"].([]any); test.kept != "" && !strings.Contains(strings.Join(anyStrings(kept), "\n"), test.kept) {
				t.Fatalf("kept %v does not mention %q", kept, test.kept)
			}
			assertNoProcessAction(t, test.fake, 0)
		})
	}
}

// disable acts on a unit name, and systemd resolves the name through its
// search path. When another file or a mask wins, a disable would change that
// unit's enablement, so only the Sessions file is removed.
func TestLinuxUninstallDoesNotDisableANameSystemdLoadsFromElsewhere(t *testing.T) {
	for _, test := range []struct {
		name string
		fake *fakeUserSystemd
		kept string
	}{
		{name: "higher-precedence file", fake: &fakeUserSystemd{held: true, enabled: true, active: true,
			shadowFragment: "/home/u/.config/systemd/user.control/sessions.service"}, kept: "user.control/sessions.service"},
		{name: "runtime unit in /run", fake: &fakeUserSystemd{held: true, enabled: true, active: true,
			shadowFragment: "/run/user/1000/systemd/user/sessions.service"}, kept: "/run/user/1000"},
		{name: "masked elsewhere", fake: &fakeUserSystemd{held: true, enabled: true, shadowMask: true}, kept: "LoadState masked"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, _ := newSessionsUnitFixture(t, test.fake)
			result := fixture.uninstall(t, 0)
			expect(t, result, map[string]any{"unit_file": "removed", "removed": fixture.unit})
			if slices.ContainsFunc(test.fake.calls, func(call string) bool { return strings.HasPrefix(call, "disable") }) || !test.fake.enabled {
				t.Fatalf("disabled a name systemd loads from elsewhere: enabled=%v calls=%v", test.fake.enabled, test.fake.calls)
			}
			if !test.fake.called("daemon-reload") {
				t.Fatalf("did not reload after removing the Sessions file: %v", test.fake.calls)
			}
			actions, _ := json.Marshal(result["actions"])
			kept, _ := json.Marshal(result["kept"])
			if !strings.Contains(string(actions), "did not run systemctl --user disable") || !strings.Contains(string(kept), test.kept) ||
				strings.Contains(string(kept), "left as found") || strings.Contains(string(kept), "unchanged") {
				t.Fatalf("actions %s / kept %s misstate the skipped disable", actions, kept)
			}
			nextMentions(t, result, "systemctl --user is-enabled -- sessions.service")
			assertNoProcessAction(t, test.fake, 0)
		})
	}
	t.Run("same file through a symlinked directory", func(t *testing.T) {
		fake := &fakeUserSystemd{held: true, enabled: true}
		fixture, _ := newSessionsUnitFixture(t, fake)
		link := filepath.Join(fixture.home, "dotfiles-user")
		if err := os.Symlink(filepath.Dir(fixture.unit), link); err != nil {
			t.Fatal(err)
		}
		fake.shadowFragment = filepath.Join(link, "sessions.service")
		expect(t, fixture.uninstall(t, 0), map[string]any{"unit_file": "removed"})
		if !fake.called("disable sessions.service") || fake.enabled {
			t.Fatalf("the Sessions file itself was loaded, so its enablement should be removed: %v", fake.calls)
		}
	})
}

func TestLinuxUninstallReportsPartialProgressAndRecoversOnRetry(t *testing.T) {
	t.Run("disable fails", func(t *testing.T) {
		fake := &fakeUserSystemd{held: true, enabled: true, active: true, fail: map[string][]error{"disable": {errors.New("exit status 1")}}}
		fixture, unit := newSessionsUnitFixture(t, fake)
		result := fixture.uninstall(t, 2)
		expect(t, result, map[string]any{"unit_file": "not_removed", "removed": ""})
		assertUnitBytes(t, fixture.unit, unit, 0o640)
		if fake.called("daemon-reload") {
			t.Fatalf("reloaded after a failed disable: %v", fake.calls)
		}
		nextMentions(t, result, "nothing was removed")
		expect(t, fixture.uninstall(t, 0), map[string]any{"unit_file": "removed"})
		assertNoProcessAction(t, fake, 0)
	})
	t.Run("remove fails", func(t *testing.T) {
		fake := &fakeUserSystemd{held: true, enabled: true, active: true}
		fixture, unit := newSessionsUnitFixture(t, fake)
		dir := filepath.Dir(fixture.unit)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		result := fixture.uninstall(t, 2)
		expect(t, result, map[string]any{"unit_file": "not_removed", "removed": ""})
		if fake.enabled || fake.called("daemon-reload") {
			t.Fatalf("enabled=%v calls=%v", fake.enabled, fake.calls)
		}
		nextMentions(t, result, "the unit file remains")
		assertUnitBytes(t, fixture.unit, unit, 0o640)
		_ = os.Chmod(dir, 0o700)
		expect(t, fixture.uninstall(t, 0), map[string]any{"unit_file": "removed", "removed": fixture.unit})
		assertNoProcessAction(t, fake, 0)
	})
	t.Run("reload fails", func(t *testing.T) {
		fake := &fakeUserSystemd{held: true, enabled: true, active: true, fail: map[string][]error{"daemon-reload": {errors.New("exit status 1")}}}
		fixture, _ := newSessionsUnitFixture(t, fake)
		result := fixture.uninstall(t, 2)
		expect(t, result, map[string]any{"unit_file": "removed", "removed": fixture.unit})
		nextMentions(t, result, "systemctl --user daemon-reload")
		if !fake.held {
			t.Fatal("fixture should still hold the removed definition")
		}
		from := len(fake.calls)
		retry := fixture.uninstall(t, 0)
		expect(t, retry, map[string]any{"unit_file": "absent", "removed": ""})
		if calls := callsSince(fake, from); !slices.Contains(calls, "daemon-reload") || slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "disable") }) {
			t.Fatalf("retry did not just complete the reload: %v", calls)
		}
		assertNoProcessAction(t, fake, 0)
	})
	t.Run("observation fails", func(t *testing.T) {
		fake := &fakeUserSystemd{held: true, enabled: true, observeErr: errors.New("exit status 1")}
		fixture, unit := newSessionsUnitFixture(t, fake)
		err := fixture.app.uninstallLinuxService(nil)
		if exitCode(err) != 2 || !strings.Contains(err.Error(), "nothing was disabled, removed, or reloaded") || len(fake.calls) != 2 {
			t.Fatalf("err = %v calls = %v", err, fake.calls)
		}
		assertUnitBytes(t, fixture.unit, unit, 0o640)
	})
}

// The text form states the same actions, kept items, and next steps as JSON.
func TestLinuxUninstallTextMatchesJSON(t *testing.T) {
	for _, failing := range []bool{false, true} {
		results, jsonHome := make([]map[string]any, 0, 1), ""
		for _, wantJSON := range []bool{true, false} {
			fake := &fakeUserSystemd{held: true, runtimeEnabled: true, active: true}
			if failing {
				fake.fail = map[string][]error{"daemon-reload": {errors.New("exit status 1")}}
			}
			fixture, _ := newSessionsUnitFixture(t, fake)
			fixture.app.wantJSON = wantJSON
			if wantJSON {
				results, jsonHome = append(results, fixture.uninstall(t, map[bool]int{false: 0, true: 2}[failing])), fixture.home
				continue
			}
			err := fixture.app.uninstallLinuxService(nil)
			text := fixture.out.String()
			if failing {
				text = err.Error()
			}
			want := anyStrings(results[0]["actions"].([]any))
			want = append(want, anyStrings(results[0]["next"].([]any))...)
			want = append(want, anyStrings(results[0]["kept"].([]any))...)
			for _, line := range want {
				if line = strings.ReplaceAll(line, jsonHome, fixture.home); !strings.Contains(text, line) {
					t.Fatalf("failing=%v text lacks %q:\n%s", failing, line, text)
				}
			}
		}
	}
}

// shellWords splits a printed command line the way a POSIX shell would for
// the quoting shellCommand produces: single-quoted spans are literal, words
// split on unquoted blanks, and unquoted && separates commands. Any other
// unquoted character a shell would treat specially fails the parse, so a
// snippet is checked without ever being run.
func shellWords(t *testing.T, line string) [][]string {
	t.Helper()
	var commands [][]string
	var words []string
	var word strings.Builder
	inWord, quoted := false, false
	flush := func() {
		if inWord {
			words, inWord = append(words, word.String()), false
			word.Reset()
		}
	}
	for index := 0; index < len(line); index++ {
		c := line[index]
		switch {
		case quoted && c == '\'':
			quoted = false
		case quoted:
			word.WriteByte(c)
		case c == '\'':
			quoted, inWord = true, true
		case c == '\\' && index+1 < len(line):
			index++
			word.WriteByte(line[index])
			inWord = true
		case c == ' ':
			flush()
		case c == '&' && index+1 < len(line) && line[index+1] == '&':
			flush()
			commands, words = append(commands, words), nil
			index++
		case strings.IndexByte(shellSafe, c) >= 0:
			word.WriteByte(c)
			inWord = true
		default:
			t.Fatalf("unquoted shell metacharacter %q at %d in %q", c, index, line)
		}
	}
	if quoted {
		t.Fatalf("unterminated quote in %q", line)
	}
	flush()
	return append(commands, words)
}

func TestLinuxUninstallPrintsRecoveryCommandsAsLiteralArguments(t *testing.T) {
	for _, home := range []string{"/tmp/space home", "/tmp/it's", "/tmp/$(touch pwned)", "/tmp/`id`", "/tmp/a;rm -rf x", "/tmp/new\nline", "-rf", "/tmp/plain"} {
		path := linuxUnitPath(home, "sessions.service")
		u := &linuxUninstall{Service: "sessions.service", UnitPath: path}
		u.keepUnrecognised("unrecognised", "")
		_, line, found := strings.Cut(u.Next[0], "by running: ")
		if !found {
			t.Fatalf("no command in %q", u.Next[0])
		}
		want := [][]string{{"systemctl", "--user", "disable", "--", "sessions.service"}, {"rm", "--", path}, {"systemctl", "--user", "daemon-reload"}}
		if got := shellWords(t, line); !slices.EqualFunc(got, want, slices.Equal[[]string]) {
			t.Fatalf("home %q: parsed %q, want %q", home, got, want)
		}
		if !strings.HasPrefix(home, "/tmp/") {
			continue
		}
		path = removeFailurePath(t, strings.TrimPrefix(home, "/tmp/"))
		u = &linuxUninstall{Service: "sessions.service", UnitPath: path}
		fake := &fakeUserSystemd{unitPath: path}
		u.removeEligible(&linuxServiceManager{systemctl: fake.systemctl}, linuxUnitObservation{loadState: "loaded", fragmentPath: path})
		if u.UnitFile != "not_removed" || len(u.Next) != 1 {
			t.Fatalf("home %q: removal did not fail as arranged: %+v", home, u)
		}
		_, line, _ = strings.Cut(u.Next[0], "or run: ")
		if got := shellWords(t, line); !slices.EqualFunc(got, [][]string{{"rm", "--", path}, {"systemctl", "--user", "daemon-reload"}}, slices.Equal[[]string]) {
			t.Fatalf("home %q: removal recovery parsed %q", home, got)
		}
	}
	if got := shellWords(t, shellCommand("-x", "")); !slices.Equal(got[0], []string{"-x", ""}) {
		t.Fatalf("leading dash and empty argument: %q", got)
	}
}

// removeFailurePath creates a real unit file in a directory named component
// that refuses the removal, so the printed recovery names a hostile path.
func removeFailurePath(t *testing.T, component string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), component, ".config", "systemd", "user")
	path := filepath.Join(dir, "sessions.service")
	if err := os.MkdirAll(dir, 0o700); err != nil || os.WriteFile(path, []byte("unit"), 0o600) != nil || os.Chmod(dir, 0o500) != nil {
		t.Fatal("hostile path fixture", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	return path
}
