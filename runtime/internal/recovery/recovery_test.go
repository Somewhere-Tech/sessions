package recovery_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

func TestScratchRecoveryScenarioClassifiesAndReopensExactlyTheOrphan(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	config := scratchConfig(root)
	store := openScratchLedger(t, root)
	launcher := prototest.NewLauncher()
	manager := sessionruntime.NewManager(config, launcher, sessionruntime.ManagerOptions{
		DisableWatchers: true, Boundaries: store.Boundaries(),
		Observations: store.Observations(), LedgerReader: store,
	})
	t.Cleanup(manager.Close)

	providers := map[string]string{
		"closed": "11111111-1111-4111-8111-111111111111",
		"orphan": "22222222-2222-4222-8222-222222222222",
		"live":   "33333333-3333-4333-8333-333333333333",
	}
	claudeRoot := filepath.Join(root, ".claude", "projects")
	for _, provider := range providers {
		writeClaudeConversation(t, claudeRoot, root, provider)
	}
	created := make(map[string]state.SessionInfo)
	for _, name := range []string{"closed", "orphan", "live"} {
		info, err := manager.Create(context.Background(), state.CreateSessionRequest{
			Cmd: "claude", Args: []string{"--resume", providers[name]}, Cwd: root, Name: name,
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		created[name] = info
	}
	if !manager.Input(context.Background(), created["orphan"].ID, "human activity") {
		t.Fatal("record orphan input")
	}
	if err := manager.RequestKill(context.Background(), created["closed"].ID, false); err != nil {
		t.Fatal(err)
	}
	orphanID := created["orphan"].ID
	for _, path := range []string{
		filepath.Join(config.RunnerStateDir, orphanID+".json"),
		filepath.Join(config.RunnerStateDir, orphanID+".sock"),
		filepath.Join(config.RunnerStateDir, orphanID+".events"),
		filepath.Join(config.RunnerStateDir, orphanID+".log"),
	} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	launcher.Runner(orphanID).Emit(proto.Event{Kind: proto.EventRunnerLost})
	waitFor(t, func() bool {
		_, present := manager.Get(orphanID)
		return !present && hasEvent(t, store, orphanID, ledger.EventRunnerLost)
	})

	report := scratchReport(t, store, manager, config, claudeRoot, filepath.Join(root, ".codex", "sessions"))
	want := map[string]ledger.Class{
		created["closed"].ID: ledger.ClassClosed,
		created["orphan"].ID: ledger.ClassUnexpectedlyLost,
		created["live"].ID:   ledger.ClassLiveManaged,
	}
	for id, class := range want {
		if got := classOf(report, id); got != class {
			t.Fatalf("lane %s class=%s, want %s", id, got, class)
		}
	}
	if len(report.Plan.Recipes) != 1 || report.Plan.Recipes[0].SourceLaneID != orphanID {
		t.Fatalf("recovery plan=%+v, want only orphan %s", report.Plan.Recipes, orphanID)
	}
	if report.Plan.Recipes[0].LastActivitySource != ledger.ActivityHumanInput {
		t.Fatalf("orphan activity source=%q, want human input", report.Plan.Recipes[0].LastActivitySource)
	}

	result := recovery.Reopen(context.Background(), report, manager, store.Observations())
	if !result.OK || len(result.Outcomes) != 1 || result.Outcomes[0].Status != recovery.ReopenCreated {
		t.Fatalf("reopen result=%+v", result)
	}
	if len(launcher.Launches) != 4 {
		t.Fatalf("launches=%d, want three originals plus one reopen", len(launcher.Launches))
	}
	if !hasEvent(t, store, orphanID, ledger.EventReopened) {
		t.Fatal("orphan source has no reopened event")
	}

	secondReport := scratchReport(t, store, manager, config, claudeRoot, filepath.Join(root, ".codex", "sessions"))
	second := recovery.Reopen(context.Background(), secondReport, manager, store.Observations())
	if !second.OK || len(second.Outcomes) != 0 || len(launcher.Launches) != 4 {
		t.Fatalf("second reopen=%+v launches=%d, want idempotent no-op", second, len(launcher.Launches))
	}
	t.Logf("classes closed=%s orphan=%s live=%s plan=%d reopened=%s launches=%d second_outcomes=%d",
		classOf(report, created["closed"].ID), classOf(report, orphanID), classOf(report, created["live"].ID),
		len(report.Plan.Recipes), result.Outcomes[0].NewLaneID, len(launcher.Launches), len(second.Outcomes))
}

func TestExplicitAdoptResolvesCodexPathAndWritesAdoptActors(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	provider := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	cwd := filepath.Join(root, "workspace")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(root, ".codex", "sessions", "2026", "07", "16", "rollout-2026-07-16T12-00-00-"+provider+".jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o700); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(map[string]any{
		"type": "session_meta", "payload": map[string]any{
			"id": provider, "cwd": cwd, "timestamp": "2026-07-16T12:00:00Z",
		},
	})
	if err := os.WriteFile(rollout, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	options := recovery.AdoptionOptions{CodexSessionsDir: filepath.Join(root, ".codex", "sessions"), ClaudeProjectsDir: filepath.Join(root, ".claude", "projects")}
	resolved, err := recovery.ResolveAdoption(provider, options)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Path != rollout || resolved.Cwd != cwd || resolved.Tool != string(state.ToolCodex) {
		t.Fatalf("resolved adoption=%+v", resolved)
	}

	store := openScratchLedger(t, root)
	launcher := prototest.NewLauncher()
	manager := sessionruntime.NewManager(scratchConfig(root), launcher, sessionruntime.ManagerOptions{
		DisableWatchers: true, Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
	})
	t.Cleanup(manager.Close)
	sourceID := "10000000-0000-4000-8000-000000000001"
	parentID := "10000000-0000-4000-8000-000000000002"
	if err := store.Boundaries().RecordCreated(context.Background(), ledger.Created{
		Meta: ledger.Meta{LaneID: sourceID},
		Name: "Landing Page", Description: "Build the public landing page",
		Tool: string(state.ToolCodex), Cwd: cwd, LaneUUID: sourceID,
		CreatorKind: ledger.CreatorUser, CreatorID: "uid:501",
	}); err != nil {
		t.Fatal(err)
	}
	result, err := recovery.Adopt(
		context.Background(), resolved, "", manager, store.Boundaries(), store.Observations(),
		recovery.AdoptOptions{Source: &recovery.AdoptSource{
			LaneID: sourceID, Name: "Landing Page", Description: "Build the public landing page",
			Tags: map[string]string{"project": "sessions"}, DisplayParentSessionID: &parentID,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.Events(context.Background(), result.LaneID)
	if err != nil {
		t.Fatal(err)
	}
	adoptCreated, adoptBound := false, false
	for _, event := range events {
		if event.Actor != ledger.ActorAdopt {
			continue
		}
		adoptCreated = adoptCreated || event.Type == ledger.EventCreated
		adoptBound = adoptBound || event.Type == ledger.EventProviderBound
	}
	if !adoptCreated || !adoptBound {
		t.Fatalf("actor=adopt facts missing: %+v", events)
	}
	var resumed state.SessionInfo
	for _, info := range manager.List(true) {
		if info.ID == result.LaneID {
			resumed = info
			break
		}
	}
	if resumed.Name != "Landing Page" || resumed.Description != "Build the public landing page" ||
		resumed.Tags["project"] != "sessions" || resumed.DisplayParentSessionID == nil ||
		*resumed.DisplayParentSessionID != parentID || resumed.Kind != state.KindCodexAppServer ||
		resumed.ConversationID != provider {
		t.Fatalf("resumed metadata = %#v", resumed)
	}
	sourceEvents, err := store.Events(context.Background(), sourceID)
	if err != nil {
		t.Fatal(err)
	}
	linked := false
	for _, event := range sourceEvents {
		if event.Type != ledger.EventReopened {
			continue
		}
		var payload map[string]string
		if json.Unmarshal(event.Payload, &payload) == nil && payload["newLaneId"] == result.LaneID {
			linked = true
		}
	}
	if !linked {
		t.Fatalf("source %s was not linked to resumed runtime %s: %+v", sourceID, result.LaneID, sourceEvents)
	}
	bad := filepath.Join(root, "unbound.jsonl")
	if err := os.WriteFile(bad, []byte(`{"cwd":"`+cwd+`"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := recovery.ResolveAdoption(bad, options); err == nil {
		t.Fatal("provider-unbound file was accepted")
	}
	t.Logf("adopt path=%s provider=%s cwd=%s lane=%s actor_created=%t actor_bound=%t",
		resolved.Path, resolved.ProviderUUID, resolved.Cwd, result.LaneID, adoptCreated, adoptBound)
}

func TestResolvePromptHistoryAdoptionUsesOnlyExactRecordedClaudeIdentity(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	provider := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	historyID := "provider-history:claude:" + provider
	resolved, err := recovery.ResolvePromptHistoryAdoption(historyID, provider, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.PromptHistoryOnly || resolved.HistoryID != historyID ||
		resolved.ProviderUUID != provider || resolved.Cwd != cwd ||
		resolved.Cmd != "claude" || len(resolved.Args) != 2 ||
		resolved.Args[0] != "--resume" || resolved.Args[1] != provider {
		t.Fatalf("resolved prompt-index adoption = %#v", resolved)
	}

	cases := []struct {
		name      string
		historyID string
		provider  string
		cwd       string
	}{
		{name: "wrong history namespace", historyID: "history:" + provider, provider: provider, cwd: cwd},
		{name: "invalid provider", historyID: historyID, provider: "not-a-uuid", cwd: cwd},
		{name: "relative workspace", historyID: historyID, provider: provider, cwd: "workspace"},
		{name: "missing workspace", historyID: historyID, provider: provider, cwd: filepath.Join(cwd, "missing")},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := recovery.ResolvePromptHistoryAdoption(
				test.historyID, test.provider, test.cwd,
			); err == nil {
				t.Fatal("unsafe prompt-index continuation was accepted")
			}
		})
	}
}

func TestAdoptPartialSuccessRepairsEachPostLaunchAnnotationWithoutRelaunch(t *testing.T) {
	cases := []struct {
		name string
		fail ledger.EventType
		want recovery.AdoptAnnotation
	}{
		{name: "adopt created", fail: ledger.EventCreated, want: recovery.AdoptAnnotationCreated},
		{name: "provider bound", fail: ledger.EventProviderBound, want: recovery.AdoptAnnotationProviderBound},
		{name: "source linked", fail: ledger.EventReopened, want: recovery.AdoptAnnotationSourceLinked},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", root)
			store := openScratchLedger(t, root)
			provider := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			sourceID := "10000000-0000-4000-8000-000000000001"
			successorID := "20000000-0000-4000-8000-000000000002"
			adoption := recovery.Adoption{
				Path: filepath.Join(root, "conversation.jsonl"), Tool: string(state.ToolCodex),
				Cwd: root, ProviderUUID: provider, Cmd: "codex", Args: []string{"resume", provider},
			}
			if err := store.Boundaries().RecordCreated(context.Background(), ledger.Created{
				Meta: ledger.Meta{LaneID: sourceID}, Name: "Landing Page", Tool: string(state.ToolCodex),
				Cwd: root, LaneUUID: sourceID, ProviderUUID: provider,
				ResumeArgv:  []string{"codex", "resume", provider},
				CreatorKind: ledger.CreatorUser, CreatorID: "uid:501",
			}); err != nil {
				t.Fatal(err)
			}
			creator := &adoptTestCreator{
				boundaries: store.Boundaries(), laneID: successorID, providerUUID: provider,
			}
			boundaries := ledger.BoundaryWriter(store.Boundaries())
			observations := ledger.ObservationWriter(store.Observations())
			if test.fail == ledger.EventCreated {
				boundaries = failAdoptCreatedBoundary{BoundaryWriter: boundaries}
			}
			if test.fail == ledger.EventProviderBound || test.fail == ledger.EventReopened {
				observations = failAdoptObservation{
					ObservationWriter: observations, fail: test.fail,
				}
			}
			source := &recovery.AdoptSource{LaneID: sourceID, Name: "Landing Page"}
			result, err := recovery.Adopt(
				context.Background(), adoption, "", creator, boundaries, observations,
				recovery.AdoptOptions{Source: source, Events: store},
			)
			if err != nil {
				t.Fatalf("adopt returned a false full failure: %v", err)
			}
			if creator.calls != 1 || result.OK || !result.Partial || result.LaneID != successorID ||
				len(result.MissingAnnotations) != 1 || result.MissingAnnotations[0] != test.want ||
				result.Repair == nil {
				t.Fatalf("partial result=%+v launches=%d", result, creator.calls)
			}

			repaired, err := recovery.RepairAdopt(
				context.Background(), adoption, "", successorID, source, store,
				store.Boundaries(), store.Observations(),
			)
			if err != nil {
				t.Fatal(err)
			}
			if !repaired.OK || repaired.Partial || len(repaired.MissingAnnotations) != 0 ||
				creator.calls != 1 {
				t.Fatalf("repair=%+v launches=%d, want complete without relaunch", repaired, creator.calls)
			}
			if !hasActorEvent(t, store, successorID, ledger.EventCreated, ledger.ActorAdopt) ||
				!hasActorEvent(t, store, successorID, ledger.EventProviderBound, ledger.ActorAdopt) ||
				!hasReopenedTarget(t, store, sourceID, successorID) {
				t.Fatalf("repair did not complete all adoption annotations for %s", test.want)
			}
		})
	}
}

type adoptTestCreator struct {
	boundaries   ledger.BoundaryWriter
	laneID       string
	providerUUID string
	calls        int
}

func (c *adoptTestCreator) Create(ctx context.Context, request state.CreateSessionRequest) (state.SessionInfo, error) {
	c.calls++
	resumeArgv := append([]string{request.Cmd}, request.Args...)
	if err := c.boundaries.RecordCreated(ctx, ledger.Created{
		Meta: ledger.Meta{LaneID: c.laneID}, Name: request.Name, Description: request.Description,
		Kind: request.Kind, Tool: string(state.ToolCodex), Cwd: request.Cwd,
		ResumeArgv: resumeArgv, LaneUUID: c.laneID, ProviderUUID: c.providerUUID,
		CreatorKind: ledger.CreatorUser, CreatorID: "uid:501",
	}); err != nil {
		return state.SessionInfo{}, err
	}
	return state.SessionInfo{ID: c.laneID, Cmd: request.Cmd, Args: request.Args, Cwd: request.Cwd}, nil
}

type failAdoptCreatedBoundary struct {
	ledger.BoundaryWriter
}

func (w failAdoptCreatedBoundary) RecordCreated(ctx context.Context, value ledger.Created) error {
	if value.Actor == ledger.ActorAdopt {
		return errors.New("injected adopt-created append failure")
	}
	return w.BoundaryWriter.RecordCreated(ctx, value)
}

type failAdoptObservation struct {
	ledger.ObservationWriter
	fail ledger.EventType
}

func (w failAdoptObservation) RecordProviderBound(ctx context.Context, value ledger.ProviderBound) error {
	if w.fail == ledger.EventProviderBound {
		return errors.New("injected provider-bound append failure")
	}
	return w.ObservationWriter.RecordProviderBound(ctx, value)
}

func (w failAdoptObservation) RecordReopened(ctx context.Context, value ledger.Reopened) error {
	if w.fail == ledger.EventReopened {
		return errors.New("injected source-link append failure")
	}
	return w.ObservationWriter.RecordReopened(ctx, value)
}

func hasActorEvent(t *testing.T, store *ledger.Store, laneID string, kind ledger.EventType, actor ledger.Actor) bool {
	t.Helper()
	events, err := store.Events(context.Background(), laneID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == kind && event.Actor == actor {
			return true
		}
	}
	return false
}

func hasReopenedTarget(t *testing.T, store *ledger.Store, sourceID, successorID string) bool {
	t.Helper()
	events, err := store.Events(context.Background(), sourceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, folded := range ledger.Fold(events) {
		if folded.LaneID == sourceID && folded.ReopenedAs == successorID {
			return true
		}
	}
	return false
}

func TestRealityProbesClassifyRuntimeOnlyRunnerAsExternal(t *testing.T) {
	root := t.TempDir()
	store := openScratchLedger(t, root)
	runnerDir := filepath.Join(root, "runners")
	if err := os.MkdirAll(runnerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	id := "99999999-9999-4999-8999-999999999999"
	socket := filepath.Join(runnerDir, id+".sock")
	info := proto.RunnerInfo{
		ID: id, Cmd: "/bin/sh", Cwd: root, Cols: 300, Rows: 50,
		CreatedAt: 1234, PID: 4321, SocketPath: socket, ProtocolVersion: proto.ProtocolVersion,
	}
	encoded, _ := json.Marshal(info)
	if err := os.WriteFile(filepath.Join(runnerDir, id+".json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := recovery.New(recovery.Options{
		Reader: store, RunnerStateDir: runnerDir,
		HelloProbe: func(context.Context, string) (proto.RunnerInfo, error) { return info, nil },
		LaunchdProbe: func(context.Context, string) (recovery.LaunchdStatus, error) {
			return recovery.LaunchdStatus{Loaded: true, Running: false}, nil
		},
	}).Report(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Lanes) != 1 || report.Lanes[0].Class != ledger.ClassExternal {
		t.Fatalf("external report=%+v", report.Lanes)
	}
	reality := report.Lanes[0].Reality
	if !reality.MetadataPresent || !reality.SocketPresent || !reality.Hello || !reality.LaunchdLoaded || reality.LaunchdRunning {
		t.Fatalf("reality=%+v", reality)
	}
	t.Logf("external=%s metadata=%t socket=%t hello=%t launchd_loaded=%t launchd_running=%t",
		report.Lanes[0].Class, reality.MetadataPresent, reality.SocketPresent, reality.Hello,
		reality.LaunchdLoaded, reality.LaunchdRunning)
}

func scratchConfig(root string) state.Config {
	return state.Config{
		Host: "127.0.0.1", Port: 8787,
		DefaultShell: "/bin/sh", DefaultCwd: root, DefaultCols: 300, DefaultRows: 50,
		StateRoot: filepath.Join(root, "sessions-state"), UserStateRoot: filepath.Join(root, "sessions-user-state"),
		RunnerStateDir: filepath.Join(root, "sessions-state", "runners"),
		TokenPath:      filepath.Join(root, "sessions-state", "token"), OpenPath: filepath.Join(root, "sessions-state", "open"),
		LaunchAgentsDir: filepath.Join(root, "LaunchAgents"), GlobalHooksPath: filepath.Join(root, "hooks.json"),
		RunnerPath: "/scratch/fake-runner",
	}
}

func openScratchLedger(t *testing.T, root string) *ledger.Store {
	t.Helper()
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(root, "ledger", "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func writeClaudeConversation(t *testing.T, projectsRoot, cwd, provider string) {
	t.Helper()
	dir := filepath.Join(projectsRoot, watch.EncodeClaudeCWD(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(map[string]any{"type": "user", "cwd": cwd, "sessionId": provider})
	if err := os.WriteFile(filepath.Join(dir, provider+".jsonl"), append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func scratchReport(
	t *testing.T,
	store *ledger.Store,
	manager *sessionruntime.Manager,
	config state.Config,
	claudeRoot, codexRoot string,
) recovery.Report {
	t.Helper()
	report, err := recovery.New(recovery.Options{
		Reader: store, RunnerStateDir: config.RunnerStateDir,
		ClaudeProjectsDir: claudeRoot, CodexSessionsDir: codexRoot,
		ManagedSessions: manager.List(false),
		LaunchdProbe: func(context.Context, string) (recovery.LaunchdStatus, error) {
			return recovery.LaunchdStatus{}, nil
		},
		HelloProbe: func(context.Context, string) (proto.RunnerInfo, error) {
			return proto.RunnerInfo{}, errors.New("scratch fake launcher has no socket")
		},
	}).Report(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func classOf(report recovery.Report, id string) ledger.Class {
	for _, lane := range report.Lanes {
		if lane.ID == id {
			return lane.Class
		}
	}
	return ""
}

func hasEvent(t *testing.T, store *ledger.Store, id string, kind ledger.EventType) bool {
	t.Helper()
	events, err := store.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == kind {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	for !condition() {
		select {
		case <-t.Context().Done():
			t.Fatal("test ended before scratch lifecycle event arrived")
		default:
			runtime.Gosched()
		}
	}
}
