package recovery_test

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
	"testing/quick"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
)

func TestRecoveryClassificationPropertiesRandomFleets(t *testing.T) {
	const seed int64 = 0xF0225EED
	var failure string
	property := func(random []byte) bool {
		// Every generated fleet contains all four classes; the random suffix
		// varies its size, order, duplicate classes, and live tombstone reality.
		fleet := append([]byte{0, 1, 2, 3}, random...)
		if len(fleet) > 68 {
			fleet = fleet[:68]
		}
		events, runtimeStates, expected := propertyFleet(fleet)
		classified := ledger.ClassifyAll(ledger.Fold(events), runtimeStates)
		if len(classified) != len(fleet) {
			failure = fmt.Sprintf("classified=%d fleet=%d", len(classified), len(fleet))
			return false
		}
		for _, classification := range classified {
			if want := expected[classification.Lane.LaneID]; classification.Class != want {
				failure = fmt.Sprintf("lane=%s class=%s want=%s", classification.Lane.LaneID, classification.Class, want)
				return false
			}
		}

		plan := ledger.BuildRecoveryPlan(classified)
		planned := make(map[string]bool, len(plan.Recipes))
		for _, recipe := range plan.Recipes {
			planned[recipe.SourceLaneID] = true
			if expected[recipe.SourceLaneID] != ledger.ClassUnexpectedlyLost {
				failure = fmt.Sprintf("unsafe lane %s class=%s entered plan", recipe.SourceLaneID, expected[recipe.SourceLaneID])
				return false
			}
		}
		for id, class := range expected {
			if planned[id] != (class == ledger.ClassUnexpectedlyLost) {
				failure = fmt.Sprintf("lane=%s class=%s planned=%t", id, class, planned[id])
				return false
			}
		}

		// Model a successful recover by appending the durable reopened fact.
		// Folding and planning the same fleet again must be a no-op.
		nextSeq := int64(len(events) + 1)
		for _, recipe := range plan.Recipes {
			payload, _ := json.Marshal(map[string]string{"newLaneId": "replacement-" + recipe.SourceLaneID})
			events = append(events, ledger.Event{
				Seq: nextSeq, EventID: fmt.Sprintf("reopened-%d", nextSeq),
				LaneID: recipe.SourceLaneID, Type: ledger.EventReopened,
				AtMS: nextSeq, Actor: ledger.ActorRecovery,
				SchemaVersion: ledger.SchemaVersion, Payload: payload,
			})
			nextSeq++
		}
		second := ledger.ClassifyAll(ledger.Fold(events), runtimeStates)
		if secondPlan := ledger.BuildRecoveryPlan(second); len(secondPlan.Recipes) != 0 {
			failure = fmt.Sprintf("repeated recovery planned %d lanes: %#v", len(secondPlan.Recipes), secondPlan.Recipes)
			return false
		}
		return true
	}
	config := &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(seed))}
	if err := quick.Check(property, config); err != nil {
		t.Fatalf("recovery property failed (seed=%#x): %v; %s", seed, err, failure)
	}
	t.Logf("seed=%#x fleets=%d classes=4 plan=orphan-only repeated_recover=no-op", seed, config.MaxCount)
}

func propertyFleet(kinds []byte) ([]ledger.Event, map[string]ledger.RuntimeState, map[string]ledger.Class) {
	events := make([]ledger.Event, 0, len(kinds)*3)
	runtimeStates := make(map[string]ledger.RuntimeState, len(kinds))
	expected := make(map[string]ledger.Class, len(kinds))
	var seq int64
	appendEvent := func(id string, kind ledger.EventType, payload any) {
		seq++
		encoded, _ := json.Marshal(payload)
		events = append(events, ledger.Event{
			Seq: seq, EventID: fmt.Sprintf("event-%d", seq), LaneID: id,
			Type: kind, AtMS: seq, Actor: ledger.ActorDaemon,
			SchemaVersion: ledger.SchemaVersion, Payload: encoded,
		})
	}
	for index, value := range kinds {
		id := fmt.Sprintf("fleet-%03d", index)
		provider := fmt.Sprintf("%08x-0000-4000-8000-%012x", index+1, index+1)
		created := map[string]any{
			"name": id, "tool": "codex", "cwd": "/tmp",
			"argv":      []string{"codex", "resume", provider},
			"lane_uuid": id, "provider_uuid": provider,
		}
		switch value % 4 {
		case 0: // live managed
			appendEvent(id, ledger.EventCreated, created)
			appendEvent(id, ledger.EventRunnerReady, struct{}{})
			runtimeStates[id] = ledger.RuntimeState{Running: true}
			expected[id] = ledger.ClassLiveManaged
		case 1: // tombstoned, including a still-running zombie
			appendEvent(id, ledger.EventCreated, created)
			appendEvent(id, ledger.EventRunnerReady, struct{}{})
			appendEvent(id, ledger.EventUserKillRequested, struct{}{})
			runtimeStates[id] = ledger.RuntimeState{Running: value&4 != 0}
			expected[id] = ledger.ClassClosed
		case 2: // orphaned
			appendEvent(id, ledger.EventCreated, created)
			appendEvent(id, ledger.EventRunnerReady, struct{}{})
			appendEvent(id, ledger.EventRunnerLost, struct{}{})
			expected[id] = ledger.ClassUnexpectedlyLost
		case 3: // external runtime, absent from the ledger
			runtimeStates[id] = ledger.RuntimeState{Running: true}
			expected[id] = ledger.ClassExternal
		}
	}
	return events, runtimeStates, expected
}
