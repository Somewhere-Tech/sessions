package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxServiceStopsOnlyDaemonAndEscapesUnitValues(t *testing.T) {
	unit := linuxServiceUnit(linuxServiceConfig{Daemon: `/tmp/space %n " quote/sessionsd`, Env: []plistEnvironment{{Key: "HOME", Value: "/tmp/%h\nline"}}})
	for _, want := range []string{"KillMode=process\n", "Restart=on-failure\n", "UMask=0077\n", `%%n`, `\"`, `%%h\nline`} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "ExecStop=") {
		t.Fatal("daemon service must not stop runners")
	}
}

func TestLinuxRuntimeStagingPreservesBytesAcrossPackageRemovalAndUpgrade(t *testing.T) {
	home, packageDir := t.TempDir(), t.TempDir()
	sources := make(map[string]string)
	for _, name := range []string{"sessions", "sessionsd", "sessions-runner"} {
		path := filepath.Join(packageDir, name)
		if err := os.WriteFile(path, []byte("first "+name), 0o700); err != nil {
			t.Fatal(err)
		}
		sources[name] = path
	}
	first, err := stageLinuxRuntime(home, sources)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sources["sessionsd"], []byte("second daemon"), 0o700); err != nil {
		t.Fatal(err)
	}
	second, err := stageLinuxRuntime(home, sources)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("upgrade must get a distinct immutable directory")
	}
	if err := os.RemoveAll(packageDir); err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(filepath.Join(first, "sessionsd"))
	if err != nil || string(bytes) != "first sessionsd" {
		t.Fatalf("old runtime lost: %q %v", bytes, err)
	}
	bytes, err = os.ReadFile(filepath.Join(second, "sessionsd"))
	if err != nil || string(bytes) != "second daemon" {
		t.Fatalf("new runtime lost: %q %v", bytes, err)
	}
}

func TestLinuxInstallReportsObservedLingerWithoutPromisingSurvival(t *testing.T) {
	for _, test := range []struct {
		output string
		err    error
		state  string
		note   []string
	}{
		{output: "yes\n", state: "enabled", note: []string{"Linger=yes", "does not protect work"}},
		{output: "no\n", state: "disabled", note: []string{"Linger=no", "Under standard systemd defaults", lingerPolicyCaveat}},
		{output: "Failed to get user: User ID 1000 is not logged in or lingering\n", err: errors.New("exit status 1"), state: "unknown", note: []string{"is not logged in or lingering", lingerPolicyCaveat}},
		{output: "", err: errors.New(`exec: "loginctl": executable file not found in $PATH`), state: "unknown", note: []string{"executable file not found"}},
		{output: "maybe\n", state: "unknown", note: []string{`Linger="maybe"`}},
	} {
		linger := userLingerFrom([]byte(test.output), test.err)
		if linger.State != test.state {
			t.Fatalf("%q/%v: got state %q, want %q", test.output, test.err, linger.State, test.state)
		}
		for _, want := range test.note {
			if !strings.Contains(linger.note(), want) {
				t.Fatalf("%q/%v: note %q does not contain %q", test.output, test.err, linger.note(), want)
			}
		}
		if test.state != "enabled" && !strings.Contains(linger.note(), `loginctl enable-linger "$USER"`) {
			t.Fatalf("%q: note does not give the next action: %q", test.output, linger.note())
		}
		if strings.Contains(linger.note(), "keep working after") || strings.Contains(linger.note(), "will survive") {
			t.Fatalf("%q: note promises future liveness: %q", test.output, linger.note())
		}
	}
}
