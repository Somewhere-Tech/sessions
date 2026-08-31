package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateRunnerPlistDisablesUnboundedLoginRestore(t *testing.T) {
	root := t.TempDir()
	agents := filepath.Join(root, "agents")
	runners := filepath.Join(root, "runners")
	id := "11111111-2222-4333-8444-555555555555"
	if err := os.MkdirAll(agents, 0o700); err != nil {
		t.Fatal(err)
	}
	path := RunnerPlistPath(agents, id)
	old := strings.Replace(plistXML(plistArgs{
		ID: id, ProgramArguments: []string{"/old/runner"}, Env: map[string]string{},
		Cwd: "/work", LogPath: filepath.Join(runners, id+".log"),
	}), `<key>RunAtLoad</key>
  <false/>
  <!-- The permit exists only while this runner belongs to the current boot.
       That preserves same-boot crash recovery without fanning every retained
       session back out after login. -->
  <key>KeepAlive</key>
  <dict>
    <key>PathState</key>
    <dict>
      <key>`+xmlEscape(filepath.Join(runners, id+".keepalive.json"))+`</key>
      <true/>
    </dict>
  </dict>`, legacyRestartPolicy, 1)
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	count, err := MigrateRunnerPlistRestartPolicies(agents, runners)
	if err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(updated), "<key>SuccessfulExit</key>") ||
		strings.Contains(string(updated), "<key>RunAtLoad</key>\n  <true/>") ||
		!strings.Contains(string(updated), filepath.Join(runners, id+".keepalive.json")) ||
		!strings.Contains(string(updated), "<key>RUNNER_RESTART_POLICY</key>\n    <string>boot-scoped</string>") {
		t.Fatalf("unsafe migrated plist:\n%s", updated)
	}
	permit, err := readRestartPermit(For(runners, id).KeepAlive)
	if err != nil || permit.BootID == "" {
		t.Fatalf("migration restart permit = %#v, %v", permit, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v err=%v", info.Mode(), err)
	}
	count, err = MigrateRunnerPlistRestartPolicies(agents, runners)
	if err != nil || count != 0 {
		t.Fatalf("second migration count=%d err=%v", count, err)
	}
}

func TestMigrateRunnerPlistLeavesUnknownPolicyAlone(t *testing.T) {
	agents := t.TempDir()
	path := RunnerPlistPath(agents, "custom")
	const custom = "<plist><dict><key>RunAtLoad</key><true/></dict></plist>"
	if err := os.WriteFile(path, []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}
	count, err := MigrateRunnerPlistRestartPolicies(agents, t.TempDir())
	if err != nil || count != 0 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != custom {
		t.Fatalf("custom plist changed: %s", got)
	}
}
