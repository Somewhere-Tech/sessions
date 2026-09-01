package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRestartPermitKeepsSameBootCrashRestartable(t *testing.T) {
	paths := For(t.TempDir(), "11111111-2222-4333-8444-555555555555")
	if err := WriteRestartPermit(paths.KeepAlive, "boot-a"); err != nil {
		t.Fatal(err)
	}
	decision, err := EvaluateRestartPermit(paths, "boot-a", 8)
	if err != nil || !decision.Allowed || decision.PinnedRestore {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	if _, err := os.Stat(paths.KeepAlive); err != nil {
		t.Fatalf("same-boot permit disappeared: %v", err)
	}
}

func TestNewBootRestoresOnlyBoundedPinnedSessionRoots(t *testing.T) {
	dir := t.TempDir()
	ids := []string{
		"11111111-2222-4333-8444-555555555551",
		"11111111-2222-4333-8444-555555555552",
		"11111111-2222-4333-8444-555555555553",
	}
	for _, id := range ids {
		if err := WriteMetadata(For(dir, id).Meta, Metadata{
			ID: id, Pinned: true, Cmd: "claude", Cwd: "/work", Cols: 80, Rows: 24,
		}); err != nil {
			t.Fatal(err)
		}
		if err := WriteRestartPermit(For(dir, id).KeepAlive, "old-boot"); err != nil {
			t.Fatal(err)
		}
	}
	first := For(dir, ids[0])
	decision, err := EvaluateRestartPermit(first, "new-boot", 2)
	if err != nil || !decision.Allowed || !decision.PinnedRestore {
		t.Fatalf("first pinned decision=%+v err=%v", decision, err)
	}
	third := For(dir, ids[2])
	decision, err = EvaluateRestartPermit(third, "new-boot", 2)
	if err != nil || decision.Allowed || decision.Reason == "" {
		t.Fatalf("third pinned decision=%+v err=%v", decision, err)
	}
	if _, err := os.Stat(third.KeepAlive); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("paused runner permit remained: %v", err)
	}
	if _, err := os.Stat(third.RestorePending); err != nil {
		t.Fatalf("paused restore was not recorded: %v", err)
	}
}

func TestNewBootNeverAutomaticallyRepeatsPinnedLane(t *testing.T) {
	dir := t.TempDir()
	id := "11111111-2222-4333-8444-555555555555"
	paths := For(dir, id)
	if err := WriteMetadata(paths.Meta, Metadata{
		ID: id, Pinned: true, Kind: KindLane, Cmd: "deploy", Cwd: "/work", Cols: 80, Rows: 24,
	}); err != nil {
		t.Fatal(err)
	}
	if err := WriteRestartPermit(paths.KeepAlive, "old-boot"); err != nil {
		t.Fatal(err)
	}
	decision, err := EvaluateRestartPermit(paths, "new-boot", 8)
	if err != nil || decision.Allowed {
		t.Fatalf("lane decision=%+v err=%v", decision, err)
	}
}

func TestPinnedSelectionIgnoresSidecars(t *testing.T) {
	dir := t.TempDir()
	id := "11111111-2222-4333-8444-555555555555"
	if err := WriteMetadata(For(dir, id).Meta, Metadata{
		ID: id, Pinned: true, Cmd: "claude", Cwd: "/work", Cols: 80, Rows: 24,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".restore-pending.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteRestartPermit(For(dir, id).KeepAlive, "old-boot"); err != nil {
		t.Fatal(err)
	}
	selected, err := pinnedRestoreIDs(dir, 8)
	if err != nil || len(selected) != 1 || selected[0] != id {
		t.Fatalf("selected=%v err=%v", selected, err)
	}
}

func TestPinnedSelectionDoesNotCountAlreadyStoppedSessions(t *testing.T) {
	dir := t.TempDir()
	stopped := "11111111-2222-4333-8444-555555555551"
	running := "11111111-2222-4333-8444-555555555552"
	for _, id := range []string{stopped, running} {
		if err := WriteMetadata(For(dir, id).Meta, Metadata{
			ID: id, Pinned: true, Cmd: "claude", Cwd: "/work", Cols: 80, Rows: 24,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteRestartPermit(For(dir, running).KeepAlive, "old-boot"); err != nil {
		t.Fatal(err)
	}
	selected, err := pinnedRestoreIDs(dir, 8)
	if err != nil || len(selected) != 1 || selected[0] != running {
		t.Fatalf("selected=%v err=%v", selected, err)
	}
}

func TestPinnedSelectionPrefersRecentActivityOverLexicographicID(t *testing.T) {
	dir := t.TempDir()
	older := "11111111-2222-4333-8444-555555555551"
	newer := "99999999-2222-4333-8444-555555555559"
	oldActivity, newActivity := int64(100), int64(900)
	for id, activity := range map[string]*int64{older: &oldActivity, newer: &newActivity} {
		if err := WriteMetadata(For(dir, id).Meta, Metadata{
			ID: id, Pinned: true, Cmd: "claude", Cwd: "/work", Cols: 80, Rows: 24,
			CreatedAt: 10, LastHumanMessageAt: activity,
		}); err != nil {
			t.Fatal(err)
		}
		if err := WriteRestartPermit(For(dir, id).KeepAlive, "old-boot"); err != nil {
			t.Fatal(err)
		}
	}
	selected, err := pinnedRestoreIDs(dir, 1)
	if err != nil || len(selected) != 1 || selected[0] != newer {
		t.Fatalf("selected=%v err=%v, want most recently active %s", selected, err, newer)
	}
}

func TestCountRestorePendingIncludesUnreadableEvidence(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"one.restore-pending.json", "two.restore-pending.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not-json"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "not-a-marker.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	count, err := CountRestorePending(dir)
	if err != nil || count != 2 {
		t.Fatalf("CountRestorePending() = %d, %v", count, err)
	}
}
