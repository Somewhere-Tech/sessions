package ledger

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"sync"
	"testing"
)

func insertProjectionEvent(t *testing.T, store *Store, event Event) {
	t.Helper()
	_, err := store.db.Exec(`INSERT INTO lane_events(seq,event_id,lane_id,type,at_ms,actor,schema_version,payload_json) VALUES (?,?,?,?,?,?,?,?)`,
		event.Seq, fmt.Sprintf("fixture-%d", event.Seq), event.LaneID, event.Type, event.AtMS, event.Actor, event.SchemaVersion, string(event.Payload))
	if err != nil {
		t.Fatal(err)
	}
}

func assertCurrentMatchesFold(t *testing.T, store *Store) []LaneState {
	t.Helper()
	events, err := store.Events(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.CurrentStates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := Fold(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("current=%+v\nfold=%+v", got, want)
	}
	return got
}

func TestCurrentStatesMatchEveryCommittedPrefixAndRecoveryBoundary(t *testing.T) {
	store := openTestStore(t, Options{})
	assertCurrentMatchesFold(t, store)
	created := createdPayload{Name: "original", Tool: "claude-code", Cwd: "/tmp", ResumeArgv: []string{"claude", "--resume", "fixture"}, ProviderUUID: "fixture", WorktreePath: "/tmp/worktree", Branch: "fixture"}
	steps := []struct {
		kind    EventType
		payload any
	}{
		{EventCreated, created}, {EventCreated, createdPayload{Name: "duplicate ignored"}},
		{EventLaunchStarted, emptyPayload{}}, {EventRunnerReady, emptyPayload{}},
		{EventProviderBound, providerPayload{ProviderUUID: "fixture", ResumeArgv: []string{"claude", "--resume", "fixture"}}},
		{EventActivity, activityPayload{Source: ActivityHumanInput}}, {EventActivity, activityPayload{Source: ActivityProviderEvent}},
		{EventActivity, activityPayload{Source: ActivitySessionInput}}, {EventIdle, emptyPayload{}},
		{EventRunnerLost, emptyPayload{}}, {EventAttached, emptyPayload{}},
		{EventRenamed, renamePayload{Name: "new name"}}, {EventDescriptionDerived, descriptionPayload{Description: "summary", Source: DescriptionFirstMessage}},
		{EventWorktreeCleanRequested, worktreeCleanRequestedPayload{WorktreePath: "/tmp/worktree", Branch: "fixture", BranchHead: "head"}},
		{EventWorktreeCleaned, worktreeCleanedPayload{WorktreePath: "/tmp/worktree", Branch: "fixture", BranchRemoved: true}},
		{EventMovedTo, movedToPayload{TargetEndpoint: "target", NewLaneID: "new"}},
		{EventMovedFrom, movedFromPayload{SourceEndpoint: "source", SourceLaneID: "old"}},
		{EventProviderRebound, providerReboundPayload{ProviderUUID: "fixture", NewLaneID: "new"}},
		{EventUserKillRequested, userKillPayload{InitiatorKind: CreatorExternal, InitiatorID: "first"}},
		{EventUserKillRequested, userKillPayload{InitiatorKind: CreatorExternal, InitiatorID: "second"}},
		{EventAttached, emptyPayload{}}, {EventReopened, reopenedPayload{NewLaneID: "new"}},
		{EventRunnerExited, map[string]any{"code": 7, "signal": "TERM"}}, {EventReaped, emptyPayload{}}, {EventArchived, emptyPayload{}},
		{EventRunnerArtifactsRetired, emptyPayload{}}, {EventDaemonRestart, emptyPayload{}}, {EventMessageRelayed, emptyPayload{}},
		{EventType("future_event"), map[string]any{"future": true}}, {EventRenamed, []string{"wrong payload shape"}},
	}
	var retained []LaneState
	for index, step := range steps {
		event := ledgerEvent(int64(index*2+1), "lane", step.kind, int64(1000-index), step.payload)
		insertProjectionEvent(t, store, event)
		current := assertCurrentMatchesFold(t, store)
		if index == 0 {
			retained = current
		}
	}
	if retained[0].Name != "original" || retained[0].UserKillRequested {
		t.Fatal("later reads mutated a prior snapshot")
	}
	current := assertCurrentMatchesFold(t, store)
	if !current[0].UserKillRequested || current[0].ManagedActive || current[0].EndInitiatorID != "first" {
		t.Fatalf("tombstone lost: %+v", current[0])
	}
	if plan := BuildRecoveryPlan(ClassifyAll(current, nil)); len(plan.Recipes) != 0 {
		t.Fatalf("closed lane reopened: %+v", plan)
	}
	current[0].ResumeArgv[0] = "mutated"
	*current[0].ExitCode = 99
	*current[0].ExitSignal = "mutated"
	assertCurrentMatchesFold(t, store)
	insertProjectionEvent(t, store, ledgerEvent(100, "", EventIdle, 1, emptyPayload{}))
	assertCurrentMatchesFold(t, store)
	if store.projection.seq != 100 || len(store.projection.lanes) != 1 {
		t.Fatal("empty lane or sequence gap corrupted projection")
	}
	reopened := openTestStore(t, Options{Path: store.Path()})
	assertCurrentMatchesFold(t, reopened)
}

func TestCurrentStatesReadsOwnAndOtherStoreCommitsAndRollback(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, Options{})
	other := openTestStore(t, Options{Path: store.Path()})
	insertProjectionEvent(t, store, createdLedgerEvent(1, "lane", "terminal", ""))
	assertCurrentMatchesFold(t, store)
	for _, writer := range []*Store{store, other} {
		if err := writer.Observations().RecordRenamed(ctx, Rename{Meta: Meta{LaneID: "lane", AtMS: 1}, Name: "committed"}); err != nil {
			t.Fatal(err)
		}
		assertCurrentMatchesFold(t, store)
	}
	for _, commit := range []bool{false, true} {
		tx, err := other.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO lane_events(event_id,lane_id,type,at_ms,actor,schema_version,payload_json) VALUES ('transaction','lane','renamed',0,'user',1,'{"name":"atomic"}'), ('transaction-kill','lane','user_kill_requested',0,'user',1,'{}')`); err != nil {
			t.Fatal(err)
		}
		before := assertCurrentMatchesFold(t, store)
		if before[0].Name == "atomic" || before[0].UserKillRequested {
			t.Fatal("uncommitted boundary leaked")
		}
		if commit {
			err = tx.Commit()
		} else {
			err = tx.Rollback()
		}
		if err != nil {
			t.Fatal(err)
		}
		after := assertCurrentMatchesFold(t, store)
		if after[0].UserKillRequested != commit {
			t.Fatal("committed boundary missed or rollback applied")
		}
	}
}

func TestCurrentStatesReadErrorDoesNotPublishPartialStateOrSkipEvents(t *testing.T) {
	store := openTestStore(t, Options{})
	insertProjectionEvent(t, store, createdLedgerEvent(1, "lane", "terminal", ""))
	before := assertCurrentMatchesFold(t, store)
	insertProjectionEvent(t, store, ledgerEvent(2, "lane", EventRenamed, 2, renamePayload{Name: "after"}))
	insertProjectionEvent(t, store, ledgerEvent(3, "lane", EventUserKillRequested, 3, emptyPayload{}))
	// This test-only view injects a scan failure AFTER a valid changed row.
	// Restore the original append-only table without modifying any event.
	if _, err := store.db.Exec(`ALTER TABLE lane_events RENAME TO projection_rows;
CREATE VIEW lane_events AS SELECT seq,lane_id,type,CASE WHEN seq=3 THEN 'not-a-time' ELSE at_ms END AS at_ms,payload_json FROM projection_rows`); err != nil {
		t.Fatal(err)
	}
	got, err := store.CurrentStates(context.Background())
	if err == nil || got != nil || store.projection.seq != 1 || !reflect.DeepEqual(before, snapshotLaneStates(store.projection.lanes)) {
		t.Fatalf("partial read published: got=%+v err=%v seq=%d", got, err, store.projection.seq)
	}
	if _, err := store.db.Exec(`DROP VIEW lane_events; ALTER TABLE projection_rows RENAME TO lane_events`); err != nil {
		t.Fatal(err)
	}
	after := assertCurrentMatchesFold(t, store)
	if after[0].Name != "after" || !after[0].UserKillRequested {
		t.Fatal("failed read skipped valid events")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := store.CurrentStates(ctx); err == nil || got != nil {
		t.Fatal("canceled read served stale success")
	}
	assertCurrentMatchesFold(t, store)
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := store.CurrentStates(context.Background()); err == nil || got != nil {
		t.Fatal("closed database served stale success")
	}
}

func TestCurrentStatesSeesSeparateProcessCommits(t *testing.T) {
	store := openTestStore(t, Options{})
	insertProjectionEvent(t, store, createdLedgerEvent(1, "lane", "terminal", ""))
	assertCurrentMatchesFold(t, store)
	command := exec.Command(os.Args[0], "-test.run=^TestCurrentStatesProcessWriter$")
	command.Env = append(os.Environ(), "SESSIONS_TEST_PROJECTION_DB="+store.Path())
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("writer: %v %s", err, output)
	}
	current := assertCurrentMatchesFold(t, store)
	if current[0].Name != "separate process" || !current[0].UserKillRequested {
		t.Fatalf("external commit missed: %+v", current)
	}
}

func TestCurrentStatesProcessWriter(t *testing.T) {
	path := os.Getenv("SESSIONS_TEST_PROJECTION_DB")
	if path == "" {
		return
	}
	store := openTestStore(t, Options{Path: path})
	ctx := context.Background()
	if err := store.Observations().RecordRenamed(ctx, Rename{Meta: Meta{LaneID: "lane"}, Name: "separate process"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Boundaries().RecordUserKill(ctx, UserKill{Meta: Meta{LaneID: "lane"}}); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentStatesConcurrentReadersAndConnectionsLoseNoEvents(t *testing.T) {
	store := openTestStore(t, Options{})
	other := openTestStore(t, Options{Path: store.Path()})
	insertProjectionEvent(t, store, createdLedgerEvent(1, "lane", "terminal", ""))
	assertCurrentMatchesFold(t, store)
	var workers sync.WaitGroup
	errors := make(chan error, 4)
	for _, writer := range []*Store{store, other} {
		workers.Add(1)
		go func(writer *Store) {
			defer workers.Done()
			for i := 0; i < 100; i++ {
				if err := writer.Observations().RecordRenamed(context.Background(), Rename{Meta: Meta{LaneID: "lane"}, Name: fmt.Sprint(i)}); err != nil {
					errors <- err
					return
				}
			}
		}(writer)
	}
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 100 {
				states, err := store.CurrentStates(context.Background())
				if err != nil {
					errors <- err
					return
				}
				states[0].Name = "caller-owned"
			}
		}()
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	assertCurrentMatchesFold(t, store)
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM lane_events`).Scan(&count); err != nil || count != 201 || store.projection.seq != 201 {
		t.Fatalf("count=%d seq=%d err=%v", count, store.projection.seq, err)
	}
}
