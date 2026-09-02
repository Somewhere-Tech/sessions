package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	selected, err := restoreCandidateIDs(dir, 8, time.Now())
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
	selected, err := restoreCandidateIDs(dir, 8, time.Now())
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
	selected, err := restoreCandidateIDs(dir, 1, time.Now())
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

// A session a person spoke to yesterday comes back after a reboot even when it
// was never pinned; one they have not touched in days stays paused, and pinned
// roots still take the slots first.
func TestNewBootRestoresRecentlySpokenToUnpinnedRoots(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	recent := now.Add(-2 * time.Hour).UnixMilli()
	stale := now.Add(-3 * 24 * time.Hour).UnixMilli()
	write := func(id string, pinned bool, human int64) {
		t.Helper()
		if err := WriteMetadata(For(dir, id).Meta, Metadata{
			ID: id, Pinned: pinned, Cmd: "claude", Cwd: "/work", Cols: 80, Rows: 24,
			LastHumanMessageAt: &human,
		}); err != nil {
			t.Fatal(err)
		}
		if err := WriteRestartPermit(For(dir, id).KeepAlive, "old-boot"); err != nil {
			t.Fatal(err)
		}
	}
	recentUnpinned := "11111111-2222-4333-8444-555555555561"
	staleUnpinned := "11111111-2222-4333-8444-555555555562"
	oldPinned := "11111111-2222-4333-8444-555555555563"
	write(recentUnpinned, false, recent)
	write(staleUnpinned, false, stale)
	write(oldPinned, true, stale)

	ids, err := restoreCandidateIDs(dir, 8, now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if !got[recentUnpinned] || !got[oldPinned] || got[staleUnpinned] {
		t.Fatalf("restore candidates = %v", ids)
	}

	// With one slot, the pinned root wins over the more recent unpinned one.
	ids, err = restoreCandidateIDs(dir, 1, now)
	if err != nil || len(ids) != 1 || ids[0] != oldPinned {
		t.Fatalf("single slot = %v err=%v", ids, err)
	}

	decision, err := EvaluateRestartPermit(For(dir, recentUnpinned), "new-boot", 8)
	if err != nil || !decision.Allowed {
		t.Fatalf("recent unpinned decision=%+v err=%v", decision, err)
	}
	decision, err = EvaluateRestartPermit(For(dir, staleUnpinned), "new-boot", 8)
	if err != nil || decision.Allowed {
		t.Fatalf("stale unpinned decision=%+v err=%v", decision, err)
	}
	if _, err := os.Stat(For(dir, staleUnpinned).RestorePending); err != nil {
		t.Fatalf("stale session was not marked resumable: %v", err)
	}
}
