package ledger

import (
	"encoding/json"
	"reflect"
	"testing"
)

func ledgerEvent(seq int64, laneID string, kind EventType, atMS int64, payload any) Event {
	encoded, _ := json.Marshal(payload)
	return Event{
		Seq: seq, EventID: laneID + "-event", LaneID: laneID, Type: kind,
		AtMS: atMS, Actor: ActorDaemon, SchemaVersion: SchemaVersion, Payload: encoded,
	}
}

func createdLedgerEvent(seq int64, laneID, tool, provider string) Event {
	argv := []string(nil)
	if provider != "" {
		argv = ResumeRecipeForProvider(tool, toolCommand(tool), provider)
	}
	return ledgerEvent(seq, laneID, EventCreated, seq*100, createdPayload{
		Name: laneID + " name", Tool: tool, Cwd: "/work/" + laneID,
		ResumeArgv: argv, LaneUUID: laneID, ProviderUUID: provider,
	})
}

func toolCommand(tool string) string {
	if tool == "claude-code" {
		return "claude"
	}
	if tool == "codex" {
		return "codex"
	}
	return "/bin/sh"
}

func TestCreatedProvenanceFoldsOnceAndRemainsImmutable(t *testing.T) {
	parent := "00000000-0000-4000-8000-000000000001"
	events := []Event{
		ledgerEvent(1, "child", EventCreated, 100, createdPayload{
			Tool: "terminal", Cwd: "/tmp", LaneUUID: "child",
			CreatorKind: CreatorSession, CreatorID: parent,
		}),
		ledgerEvent(2, "child", EventCreated, 200, createdPayload{
			Tool: "terminal", Cwd: "/tmp", LaneUUID: "child",
			CreatorKind: CreatorExternal, CreatorID: "attempted-transfer",
		}),
	}
	got := Fold(events)
	if len(got) != 1 || got[0].CreatorKind != CreatorSession || got[0].CreatorID != parent {
		t.Fatalf("folded creator = %#v", got)
	}

	legacy := Fold([]Event{createdLedgerEvent(1, "legacy", "terminal", "")})
	if len(legacy) != 1 || !legacy[0].Created || legacy[0].CreatorKind != "" || legacy[0].CreatorID != "" {
		t.Fatalf("legacy created event did not remain readable: %#v", legacy)
	}
}

func TestExplicitCreatedDescriptionWinsOverDerivedFallback(t *testing.T) {
	events := []Event{
		ledgerEvent(1, "lane", EventCreated, 100, createdPayload{
			Tool: "terminal", Cwd: "/tmp", LaneUUID: "lane",
			Description: "explicit purpose", DescriptionSource: DescriptionExplicit,
		}),
		ledgerEvent(2, "lane", EventDescriptionDerived, 200, descriptionPayload{
			Description: "later first message", Source: DescriptionFirstMessage,
		}),
	}
	got := Fold(events)
	if len(got) != 1 || got[0].Description != "explicit purpose" || got[0].DescriptionSource != DescriptionExplicit {
		t.Fatalf("folded description = %#v", got)
	}

	derived := Fold([]Event{
		createdLedgerEvent(1, "fallback", "terminal", ""),
		ledgerEvent(2, "fallback", EventDescriptionDerived, 200, descriptionPayload{
			Description: "first user request", Source: DescriptionFirstMessage,
		}),
		ledgerEvent(3, "fallback", EventDescriptionDerived, 300, descriptionPayload{
			Description: "later request must not replace it", Source: DescriptionFirstMessage,
		}),
	})
	if len(derived) != 1 || derived[0].Description != "first user request" || derived[0].DescriptionSource != DescriptionFirstMessage {
		t.Fatalf("derived description = %#v", derived)
	}
}

func TestWorktreeCleanupFactsFoldOnlyForCreatedIdentity(t *testing.T) {
	events := []Event{
		ledgerEvent(1, "lane", EventCreated, 100, createdPayload{
			Tool: "terminal", Cwd: "/tmp/project-wt/lane", LaneUUID: "lane",
			WorktreePath: "/tmp/project-wt/lane", Branch: "sessions/lane", Base: "main", SourceRepo: "/tmp/project",
		}),
		ledgerEvent(2, "lane", EventWorktreeCleanRequested, 200, worktreeCleanRequestedPayload{
			WorktreePath: "/tmp/other", Branch: "sessions/other", BranchHead: "wrong",
		}),
		ledgerEvent(3, "lane", EventWorktreeCleanRequested, 300, worktreeCleanRequestedPayload{
			WorktreePath: "/tmp/project-wt/lane", Branch: "sessions/lane", BranchHead: "abc123",
		}),
		ledgerEvent(4, "lane", EventWorktreeCleaned, 400, worktreeCleanedPayload{
			WorktreePath: "/tmp/project-wt/lane", Branch: "sessions/lane", BranchRemoved: true,
		}),
	}
	state := Fold(events)[0]
	if !state.WorktreeCleanRequested || state.WorktreeCleanBranchHead != "abc123" ||
		!state.WorktreeCleaned || state.WorktreeCleanedAtMS != 400 || !state.WorktreeBranchRemoved {
		t.Fatalf("worktree cleanup state = %#v", state)
	}
}

func TestTombstoneWinsForever(t *testing.T) {
	provider := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	events := []Event{
		createdLedgerEvent(1, "lane", "claude-code", provider),
		ledgerEvent(2, "lane", EventRunnerReady, 200, emptyPayload{}),
		ledgerEvent(3, "lane", EventUserKillRequested, 300, emptyPayload{}),
		ledgerEvent(4, "lane", EventRunnerLost, 400, emptyPayload{}),
		ledgerEvent(5, "lane", EventAttached, 500, emptyPayload{}),
		ledgerEvent(6, "lane", EventReopened, 600, reopenedPayload{NewLaneID: "new-lane"}),
	}
	state := Fold(events)[0]
	classification := ClassifyLane(state, RuntimeState{})
	if classification.Class != ClassClosed || !state.UserKillRequested || state.ManagedActive {
		t.Fatalf("classification=%#v state=%#v", classification, state)
	}
	if plan := BuildRecoveryPlan([]Classification{classification}); len(plan.Recipes) != 0 {
		t.Fatalf("tombstoned lane entered recovery plan: %#v", plan)
	}
}

func TestUserKillAttributionFoldsIntoClosedLane(t *testing.T) {
	events := []Event{
		createdLedgerEvent(1, "landing-page", "claude-code", ""),
		ledgerEvent(2, "landing-page", EventUserKillRequested, 200, userKillPayload{
			InitiatorKind: CreatorSession,
			InitiatorID:   "pm-claude",
			Client:        "sessions-cli",
			Reason:        "Kill completed lanes",
			OperationID:   "batch-123",
		}),
	}
	state := Fold(events)[0]
	if !state.UserKillRequested || state.ClosedAtMS != 200 {
		t.Fatalf("kill lifecycle did not fold: %#v", state)
	}
	if state.EndInitiatorKind != CreatorSession || state.EndInitiatorID != "pm-claude" ||
		state.EndClient != "sessions-cli" || state.EndReason != "Kill completed lanes" ||
		state.EndOperationID != "batch-123" {
		t.Fatalf("kill attribution = %#v", state)
	}
}

func TestFirstUserKillRequestOwnsAttribution(t *testing.T) {
	events := []Event{
		createdLedgerEvent(1, "landing-page", "claude-code", ""),
		ledgerEvent(2, "landing-page", EventUserKillRequested, 200, userKillPayload{
			InitiatorKind: CreatorSession,
			InitiatorID:   "00000000-0000-4000-8000-000000000123",
			Client:        "sessions-cli",
			Reason:        "Kill completed lanes",
			OperationID:   "batch-123",
		}),
		ledgerEvent(3, "landing-page", EventUserKillRequested, 300, userKillPayload{
			InitiatorKind: CreatorUser,
			InitiatorID:   "uid:501",
			Client:        "sessions-desktop",
			Reason:        "retry",
			OperationID:   "batch-456",
		}),
	}
	state := Fold(events)[0]
	if state.ClosedAtMS != 200 || state.EndInitiatorKind != CreatorSession ||
		state.EndInitiatorID != "00000000-0000-4000-8000-000000000123" ||
		state.EndClient != "sessions-cli" || state.EndReason != "Kill completed lanes" ||
		state.EndOperationID != "batch-123" {
		t.Fatalf("first kill attribution was overwritten: %#v", state)
	}
}

func TestReconnectClearsTransientLostTimestampBeforeRealExit(t *testing.T) {
	state := Fold([]Event{
		createdLedgerEvent(1, "reconnected", "codex", ""),
		ledgerEvent(2, "reconnected", EventRunnerReady, 100, emptyPayload{}),
		ledgerEvent(3, "reconnected", EventRunnerLost, 200, emptyPayload{}),
		ledgerEvent(4, "reconnected", EventAttached, 300, emptyPayload{}),
		ledgerEvent(5, "reconnected", EventRunnerExited, 400, runnerExitPayload{}),
	})[0]
	if state.RunnerLost || !state.RunnerExited || state.ClosedAtMS != 400 {
		t.Fatalf("reconnected lifecycle = %#v", state)
	}
}

func TestLateAttachDoesNotClearContinuedTimestamp(t *testing.T) {
	state := Fold([]Event{
		createdLedgerEvent(1, "continued", "codex", ""),
		ledgerEvent(2, "continued", EventRunnerLost, 200, emptyPayload{}),
		ledgerEvent(3, "continued", EventReopened, 300, reopenedPayload{NewLaneID: "replacement"}),
		ledgerEvent(4, "continued", EventAttached, 400, emptyPayload{}),
	})[0]
	if state.ReopenedAs != "replacement" || state.ClosedAtMS != 200 {
		t.Fatalf("continued lifecycle = %#v", state)
	}
}

func TestTombstoneWithoutCreatedFactStillBeatsRuntimeEvidence(t *testing.T) {
	state := Fold([]Event{
		ledgerEvent(1, "partial-lane", EventUserKillRequested, 100, emptyPayload{}),
		ledgerEvent(2, "partial-lane", EventRunnerReady, 200, emptyPayload{}),
	})[0]
	classification := ClassifyLane(state, RuntimeState{Running: true})
	if classification.Class != ClassClosed || !state.UserKillRequested || state.ManagedActive {
		t.Fatalf("classification=%#v state=%#v", classification, state)
	}
	if plan := BuildRecoveryPlan([]Classification{classification}); len(plan.Recipes) != 0 {
		t.Fatalf("partial tombstoned lane entered recovery plan: %#v", plan)
	}
}

func TestClassificationTableAllClassesAndAnomalies(t *testing.T) {
	provider := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	tests := []struct {
		name      string
		events    []Event
		runtime   map[string]RuntimeState
		laneID    string
		wantClass Class
		want      []Anomaly
	}{
		{
			name: "live managed", laneID: "live", wantClass: ClassLiveManaged,
			events:  []Event{createdLedgerEvent(1, "live", "claude-code", provider), ledgerEvent(2, "live", EventRunnerReady, 200, emptyPayload{})},
			runtime: map[string]RuntimeState{"live": {Running: true}},
		},
		{
			name: "closed", laneID: "closed", wantClass: ClassClosed,
			events: []Event{createdLedgerEvent(1, "closed", "terminal", ""), ledgerEvent(2, "closed", EventRunnerReady, 200, emptyPayload{}), ledgerEvent(3, "closed", EventRunnerExited, 300, runnerExitPayload{})},
		},
		{
			name: "unexpectedly lost", laneID: "lost", wantClass: ClassUnexpectedlyLost,
			events: []Event{createdLedgerEvent(1, "lost", "claude-code", provider), ledgerEvent(2, "lost", EventRunnerReady, 200, emptyPayload{}), ledgerEvent(3, "lost", EventRunnerLost, 300, emptyPayload{})},
		},
		{
			name: "external", laneID: "external", wantClass: ClassExternal,
			runtime: map[string]RuntimeState{"external": {Running: true}},
		},
		{
			name: "closed but running", laneID: "zombie", wantClass: ClassClosed,
			events:  []Event{createdLedgerEvent(1, "zombie", "terminal", ""), ledgerEvent(2, "zombie", EventRunnerReady, 200, emptyPayload{}), ledgerEvent(3, "zombie", EventUserKillRequested, 300, emptyPayload{})},
			runtime: map[string]RuntimeState{"zombie": {Running: true}},
			want:    []Anomaly{AnomalyClosedButRunning},
		},
		{
			name: "never ready and provider unbound", laneID: "unready", wantClass: ClassUnexpectedlyLost,
			events: []Event{createdLedgerEvent(1, "unready", "codex", "")},
			want:   []Anomaly{AnomalyNeverBecameReady, AnomalyProviderUnbound},
		},
		{
			name: "resume source missing", laneID: "missing", wantClass: ClassUnexpectedlyLost,
			events:  []Event{createdLedgerEvent(1, "missing", "claude-code", provider), ledgerEvent(2, "missing", EventRunnerReady, 200, emptyPayload{})},
			runtime: map[string]RuntimeState{"missing": {ResumeSourceKnown: true, ResumeSourceExists: false}},
			want:    []Anomaly{AnomalyResumeSourceMissing},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			classified := ClassifyAll(Fold(test.events), test.runtime)
			var got Classification
			for _, candidate := range classified {
				if candidate.Lane.LaneID == test.laneID {
					got = candidate
				}
			}
			if got.Class != test.wantClass || !reflect.DeepEqual(got.Anomalies, test.want) {
				t.Fatalf("got class=%q anomalies=%q, want class=%q anomalies=%q", got.Class, got.Anomalies, test.wantClass, test.want)
			}
			t.Logf("class=%s anomalies=%v", got.Class, got.Anomalies)
		})
	}
}

func TestRecoveryPlanUsesOnlyActivityAndRanksMostRecentFirst(t *testing.T) {
	providerA := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	providerB := "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"
	events := []Event{
		createdLedgerEvent(1, "older", "claude-code", providerA),
		ledgerEvent(2, "older", EventRunnerReady, 200, emptyPayload{}),
		ledgerEvent(3, "older", EventActivity, 1000, activityPayload{Source: ActivityHumanInput}),
		createdLedgerEvent(4, "newer", "codex", providerB),
		ledgerEvent(5, "newer", EventRunnerReady, 500, emptyPayload{}),
		ledgerEvent(6, "newer", EventAttached, 9000, emptyPayload{}), // not activity
		ledgerEvent(7, "newer", EventActivity, 2000, activityPayload{Source: ActivityProviderEvent}),
	}
	classified := ClassifyAll(Fold(events), nil)
	plan := BuildRecoveryPlan(classified)
	if len(plan.Recipes) != 2 || plan.Recipes[0].SourceLaneID != "newer" || plan.Recipes[1].SourceLaneID != "older" {
		t.Fatalf("plan order=%#v", plan.Recipes)
	}
	if plan.Recipes[0].LastActivityAtMS != 2000 || plan.Recipes[0].Cmd != "codex" ||
		!reflect.DeepEqual(plan.Recipes[0].Args, []string{"resume", providerB}) {
		t.Fatalf("newer recipe=%#v", plan.Recipes[0])
	}
	missingState := Fold([]Event{
		createdLedgerEvent(1, "missing", "claude-code", providerA),
		ledgerEvent(2, "missing", EventRunnerReady, 200, emptyPayload{}),
	})[0]
	missing := ClassifyLane(missingState, RuntimeState{ResumeSourceKnown: true})
	blocked := BuildRecoveryPlan([]Classification{missing})
	if len(blocked.Recipes) != 1 || !blocked.Recipes[0].Blocked {
		t.Fatalf("missing resume source was not retained as blocked: %#v", blocked)
	}
}

func TestRecoveryPlanPrefersHumanInputAtEqualRecency(t *testing.T) {
	providerA := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	providerB := "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"
	events := []Event{
		createdLedgerEvent(1, "provider", "claude-code", providerA),
		ledgerEvent(2, "provider", EventRunnerReady, 200, emptyPayload{}),
		ledgerEvent(3, "provider", EventActivity, 1000, activityPayload{Source: ActivityProviderEvent}),
		createdLedgerEvent(4, "human", "codex", providerB),
		ledgerEvent(5, "human", EventRunnerReady, 500, emptyPayload{}),
		ledgerEvent(6, "human", EventActivity, 1000, activityPayload{Source: ActivityHumanInput}),
	}
	plan := BuildRecoveryPlan(ClassifyAll(Fold(events), nil))
	if len(plan.Recipes) != 2 || plan.Recipes[0].SourceLaneID != "human" ||
		plan.Recipes[0].LastActivitySource != ActivityHumanInput {
		t.Fatalf("equal-recency plan=%+v, want human input first", plan.Recipes)
	}
}
