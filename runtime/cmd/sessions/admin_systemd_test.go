package main

import (
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
