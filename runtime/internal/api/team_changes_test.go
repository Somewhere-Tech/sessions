package api

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/verdict"
)

func TestTeamDeltaOnlyReturnsMeaningfulChanges(t *testing.T) {
	var cache teamChanges
	now := time.Now()
	initial := teamListing{Members: []teamMember{{ID: "a", State: "working", UpdatedAt: 1}, {ID: "b", State: "idle"}}}
	if err := cache.apply("manager", "", &initial, now); err != nil {
		t.Fatal(err)
	}
	same := teamListing{Members: []teamMember{{ID: "b", State: "idle"}, {ID: "a", State: "working", UpdatedAt: 999}}}
	if err := cache.apply("manager", initial.NextCursor, &same, now); err != nil {
		t.Fatal(err)
	}
	if len(same.Members) != 0 || !same.Delta || same.Total != 2 || same.NextCursor != initial.NextCursor {
		t.Fatalf("unchanged = %+v", same)
	}
	changed := teamListing{Members: []teamMember{{ID: "a", State: "failed", Waiting: "Sign in"}}, NeedsInput: 1}
	if err := cache.apply("manager", initial.NextCursor, &changed, now); err != nil {
		t.Fatal(err)
	}
	if len(changed.Members) != 1 || len(changed.Removed) != 1 || changed.Removed[0] != "b" || changed.NeedsInput != 1 {
		t.Fatalf("changed = %+v", changed)
	}
	// Re-reading the same cursor is not a destructive acknowledgement.
	retry := teamListing{Members: []teamMember{{ID: "a", State: "failed", Waiting: "Sign in"}}}
	if err := cache.apply("manager", initial.NextCursor, &retry, now); err != nil || len(retry.Members) != 1 {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
}

func TestTeamCursorScopesExpiryAndBounds(t *testing.T) {
	var cache teamChanges
	now := time.Now()
	listing := teamListing{Members: []teamMember{}}
	if err := cache.apply("a", "", &listing, now); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		caller string
		at     time.Time
	}{{"b", now}, {"a", now.Add(teamCursorTTL)}} {
		fresh := teamListing{Members: []teamMember{}}
		if err := cache.apply(test.caller, listing.NextCursor, &fresh, test.at); !errors.Is(err, errTeamCursor) {
			t.Fatalf("err=%v", err)
		}
	}
	huge := teamListing{Members: make([]teamMember, teamMemberMax+1)}
	if err := cache.apply("a", "", &huge, now); err == nil {
		t.Fatal("oversize baseline accepted")
	}
	for i := 0; i < teamCursorMax+2; i++ {
		if _, err := cache.save(teamSnapshot{caller: string(rune(i + 1)), at: now.Add(time.Duration(i)), rows: map[string][32]byte{}}); err != nil {
			t.Fatal(err)
		}
	}
	if len(cache.snapshots) > teamCursorMax {
		t.Fatal("cache is unbounded")
	}
}

func TestReceiptKeepsUnknownsAndAttribution(t *testing.T) {
	record := verdict.Record{Verdict: "done", Seq: 3, Meta: map[string]any{"handoff": map[string]any{
		"summary": "Added tests", "commits": []any{"abc123"}, "push": "not-pushed",
		"tests": []any{"go test ./... passed"}, "artifacts": []any{"report.md"}, "remaining": []any{"native test"},
	}}}
	receipt := receiptFrom(record, teamMember{Cwd: "/work", Branch: "topic"})
	if receipt.Source != "agent-reported" || receipt.Seq != 3 || receipt.Push != "not-pushed" || len(receipt.Commits) != 1 || len(receipt.Tests) != 1 {
		t.Fatalf("receipt=%+v", receipt)
	}
	unknown := receiptFrom(verdict.Record{Verdict: "pass"}, teamMember{})
	if unknown.Push != "unknown" || len(unknown.Tests) != 0 {
		t.Fatalf("invented proof: %+v", unknown)
	}
	record.SkippedRecords = 1
	if receiptFrom(record, teamMember{}).Detail == "" {
		t.Fatal("damaged receipt not disclosed")
	}
	record.Meta["handoff"] = "not an object"
	if receiptFrom(record, teamMember{}).Detail == "" {
		t.Fatal("invalid receipt not disclosed")
	}
}

func TestReceiptReadFailureDoesNotLookLikeNoReport(t *testing.T) {
	t.Setenv("SESSIONS_LEDGER_PATH", filepath.Join(t.TempDir(), "ledger.db"))
	root := t.TempDir()
	server := &Server{config: state.Config{RunnerStateDir: root}}
	if err := os.WriteFile(filepath.Join(root, "worker.verdicts.jsonl"), []byte("{broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	listing := teamListing{Members: []teamMember{{ID: "worker"}, {ID: "missing"}}}
	server.attachTeamReceipts(&listing)
	if listing.Members[0].Handoff.Detail == "" || listing.Members[1].Handoff.Detail != "" {
		t.Fatalf("receipts=%+v %+v", listing.Members[0].Handoff, listing.Members[1].Handoff)
	}
}

func TestSharedCheckoutIncludesSubdirectoriesAndUntrackedEdits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	root := t.TempDir()
	if out, err := exec.Command("git", "init", root).CombinedOutput(); err != nil {
		t.Fatalf("init: %s %v", out, err)
	}
	subdir := filepath.Join(root, "sub")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "untracked.txt"), []byte("preserve me"), 0o600); err != nil {
		t.Fatal(err)
	}
	infos := []state.SessionInfo{{ID: "a", Cwd: root}, {ID: "b", Cwd: subdir}, {ID: "ended", Cwd: root, Exited: true}}
	listing := teamListing{Members: []teamMember{{ID: "a", Cwd: root}}}
	attachCheckoutWarnings(context.Background(), &listing, infos)
	warning := listing.Members[0].Checkout
	if warning == nil || warning.Dirty == nil || !*warning.Dirty || len(warning.Sessions) != 2 {
		t.Fatalf("warning=%+v", warning)
	}
	if contents, err := os.ReadFile(filepath.Join(root, "untracked.txt")); err != nil || string(contents) != "preserve me" {
		t.Fatal("inspection changed checkout")
	}
}
