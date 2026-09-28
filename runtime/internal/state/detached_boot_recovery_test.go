package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetachedBootRecoveryPreservesHistoryAndRequiresExplicitWake(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"old", "same", "no-launch"} {
		paths := For(dir, id)
		if err := os.WriteFile(paths.Meta, []byte(`{"id":"`+id+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(paths.Events, []byte("history"), 0o600); err != nil {
			t.Fatal(err)
		}
		boot := "before-reboot"
		if id == "same" {
			boot = "current-boot"
		}
		if err := WriteRestartPermit(paths.KeepAlive, boot); err != nil {
			t.Fatal(err)
		}
		if id != "no-launch" {
			if err := writeLaunchSpec(paths.Launch, launchSpec{ID: id, Program: []string{"/nonexistent-must-never-launch"}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := 0; i < 2; i++ {
		if err := markDetachedBootRecovery(dir, "current-boot"); err != nil {
			t.Fatal(err)
		}
	}
	paths := For(dir, "old")
	pending, err := ReadRestorePending(paths.RestorePending)
	if err != nil || pending.SessionID != "old" {
		t.Fatalf("missing paused state: %+v %v", pending, err)
	}
	for _, path := range []string{paths.Meta, paths.Events, paths.Launch, paths.KeepAlive} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("lost retained evidence %s: %v", path, err)
		}
	}
	for _, id := range []string{"same", "no-launch"} {
		if _, err := os.Stat(For(dir, id).RestorePending); !os.IsNotExist(err) {
			t.Fatalf("incorrect paused marker for %s: %v", id, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "old.sock")); !os.IsNotExist(err) {
		t.Fatal("recovery must not start any runner")
	}
}
