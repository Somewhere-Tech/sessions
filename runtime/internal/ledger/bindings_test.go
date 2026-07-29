package ledger

import (
	"context"
	"testing"
)

func TestLiveAndMovedBindingQueriesFoldLifecycleAndRebindFacts(t *testing.T) {
	store := openTestStore(t, Options{})
	ctx := context.Background()
	provider := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	resume := []string{"claude", "--resume", provider}
	recordLive := func(laneID, name string, atMS int64) {
		t.Helper()
		if err := store.Boundaries().RecordCreated(ctx, Created{
			Meta: Meta{LaneID: laneID, AtMS: atMS}, Name: name, Tool: "claude-code", Cwd: "/tmp",
			ResumeArgv: resume, LaneUUID: laneID, ProviderUUID: provider,
			CreatorKind: CreatorExternal, CreatorID: "bindings-test",
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.Observations().RecordRunnerReady(ctx, Observation{Meta: Meta{LaneID: laneID, AtMS: atMS + 1}}); err != nil {
			t.Fatal(err)
		}
	}

	recordLive("old-lane", "old name", 100)
	binding, err := store.LiveBindingFor(ctx, provider)
	if err != nil {
		t.Fatal(err)
	}
	if binding == nil || binding.SessionID != "old-lane" || binding.Name != "old name" || binding.Kind != "claude-code" {
		t.Fatalf("live binding = %#v", binding)
	}
	if err := store.Migrations().RecordMovedTo(ctx, MovedTo{
		Meta: Meta{LaneID: "old-lane", AtMS: 200}, TargetEndpoint: "studio.example", NewLaneID: "remote-lane",
	}); err != nil {
		t.Fatal(err)
	}
	moved, err := store.MovedBinding(ctx, provider)
	if err != nil {
		t.Fatal(err)
	}
	if moved == nil || moved.Machine != "studio.example" || moved.SourceSessionID != "old-lane" {
		t.Fatalf("moved binding = %#v", moved)
	}

	if err := store.Boundaries().RecordCreated(ctx, Created{
		Meta: Meta{LaneID: "new-lane", AtMS: 300}, Name: "new name", Tool: "claude-code", Cwd: "/tmp",
		ResumeArgv: resume, LaneUUID: "new-lane", ProviderUUID: provider,
		CreatorKind: CreatorExternal, CreatorID: "bindings-test",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Boundaries().RecordProviderRebound(ctx, ProviderRebound{
		Meta: Meta{LaneID: "old-lane", AtMS: 301}, ProviderUUID: provider, NewLaneID: "new-lane",
	}); err != nil {
		t.Fatal(err)
	}
	// Until the replacement is ready, the pre-launch rebind fact must not
	// make the live old lane disappear.
	binding, err = store.LiveBindingFor(ctx, provider)
	if err != nil || binding == nil || binding.SessionID != "old-lane" {
		t.Fatalf("pre-ready binding = %#v err=%v", binding, err)
	}
	if err := store.Observations().RecordRunnerReady(ctx, Observation{Meta: Meta{LaneID: "new-lane", AtMS: 302}}); err != nil {
		t.Fatal(err)
	}
	binding, err = store.LiveBindingFor(ctx, provider)
	if err != nil || binding == nil || binding.SessionID != "new-lane" {
		t.Fatalf("rebound binding = %#v err=%v", binding, err)
	}
	moved, err = store.MovedBinding(ctx, provider)
	if err != nil || moved != nil {
		t.Fatalf("active local rebound still reported moved = %#v err=%v", moved, err)
	}
	if err := store.Boundaries().RecordUserKill(ctx, UserKill{Meta: Meta{LaneID: "new-lane", AtMS: 400}}); err != nil {
		t.Fatal(err)
	}
	binding, err = store.LiveBindingFor(ctx, provider)
	if err != nil || binding == nil || binding.SessionID != "old-lane" {
		t.Fatalf("binding after replacement kill = %#v err=%v", binding, err)
	}
	moved, err = store.MovedBinding(ctx, provider)
	if err != nil || moved == nil || moved.Machine != "studio.example" {
		t.Fatalf("moved binding after replacement kill = %#v err=%v", moved, err)
	}
}
