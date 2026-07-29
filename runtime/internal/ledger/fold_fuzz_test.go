package ledger

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func FuzzEventFold(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		events := fuzzEvents(data)
		before := cloneEvents(events)

		first := Fold(events)
		second := Fold(events)
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("fold is nondeterministic:\nfirst=%#v\nsecond=%#v", first, second)
		}
		if !reflect.DeepEqual(events, before) {
			t.Fatalf("fold mutated its input:\nbefore=%#v\nafter=%#v", before, events)
		}

		reversed := cloneEvents(events)
		for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
			reversed[left], reversed[right] = reversed[right], reversed[left]
		}
		if reordered := Fold(reversed); !reflect.DeepEqual(first, reordered) {
			t.Fatalf("fold depends on input order with unique seq values:\nforward=%#v\nreversed=%#v", first, reordered)
		}
		runtimeStates := make(map[string]RuntimeState, len(first))
		for index, state := range first {
			runtimeStates[state.LaneID] = RuntimeState{Running: dataBit(data, index)}
		}
		firstClassification := ClassifyAll(first, runtimeStates)
		secondClassification := ClassifyAll(second, runtimeStates)
		if !reflect.DeepEqual(firstClassification, secondClassification) {
			t.Fatalf("classification is nondeterministic:\nfirst=%#v\nsecond=%#v", firstClassification, secondClassification)
		}
		if firstPlan, secondPlan := BuildRecoveryPlan(firstClassification), BuildRecoveryPlan(secondClassification); !reflect.DeepEqual(firstPlan, secondPlan) {
			t.Fatalf("recovery plan is nondeterministic:\nfirst=%#v\nsecond=%#v", firstPlan, secondPlan)
		}

		tombstoned := make(map[string]bool)
		for _, event := range events {
			if event.LaneID != "" && event.Type == EventUserKillRequested {
				tombstoned[event.LaneID] = true
			}
		}
		for index, state := range first {
			if state.LaneID == "" {
				t.Fatalf("fold emitted an empty lane id: %#v", state)
			}
			if index > 0 && first[index-1].LaneID >= state.LaneID {
				t.Fatalf("fold output is not uniquely sorted: %#v", first)
			}
			closed := state.UserKillRequested || state.RunnerExited || state.Reaped || state.ReopenedAs != ""
			if closed && state.ManagedActive {
				t.Fatalf("terminal lane became managed-active: %#v", state)
			}
			if !tombstoned[state.LaneID] {
				continue
			}
			classification := ClassifyLane(state, RuntimeState{Running: dataBit(data, index)})
			if !state.UserKillRequested || classification.Class != ClassClosed {
				t.Fatalf("tombstone did not win: state=%#v classification=%#v", state, classification)
			}
			if plan := BuildRecoveryPlan([]Classification{classification}); len(plan.Recipes) != 0 {
				t.Fatalf("tombstoned lane entered recovery plan: %#v", plan)
			}
		}
	})
}

func fuzzEvents(data []byte) []Event {
	const recordBytes = 6
	count := min(len(data)/recordBytes, 64)
	events := make([]Event, 0, count)
	for index := 0; index < count; index++ {
		record := data[index*recordBytes : (index+1)*recordBytes]
		laneID := fmt.Sprintf("lane-%d", record[0]%4)
		if record[0] == 0xff {
			laneID = ""
		}
		kind := fuzzEventType(record[1])
		events = append(events, Event{
			Seq:           int64(index + 1),
			EventID:       fmt.Sprintf("event-%d", record[4]%4),
			LaneID:        laneID,
			Type:          kind,
			AtMS:          int64(int8(record[3])),
			Actor:         Actor(record[5]),
			SchemaVersion: int(record[2]),
			Payload:       fuzzPayload(kind, record[2], record),
		})
	}
	if len(data) > 0 && data[0]&0x80 != 0 {
		for left, right := 0, len(events)-1; left < right; left, right = left+1, right-1 {
			events[left], events[right] = events[right], events[left]
		}
	}
	return events
}

func fuzzEventType(value byte) EventType {
	types := [...]EventType{
		EventCreated, EventRunnerReady, EventUserKillRequested, EventRunnerLost,
		EventAttached, EventReopened, EventActivity, EventRenamed,
		EventProviderBound, EventRunnerExited, EventReaped, EventLaunchStarted,
		EventIdle, EventDaemonRestart, EventMovedTo, EventMovedFrom, EventProviderRebound,
	}
	if int(value)%18 == 17 {
		return EventType(fmt.Sprintf("unknown-%02x", value))
	}
	return types[int(value)%len(types)]
}

func fuzzPayload(kind EventType, choice byte, raw []byte) json.RawMessage {
	if choice%5 == 4 {
		return append(json.RawMessage(nil), raw...)
	}
	if choice%5 == 3 {
		return json.RawMessage(`{"truncated":`)
	}
	switch kind {
	case EventCreated:
		if choice%3 == 0 {
			return json.RawMessage(`{"name":"seed","tool":"codex","cwd":"/tmp","argv":["codex","resume","aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"],"lane_uuid":"lane-0","provider_uuid":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
		}
		return json.RawMessage(`{"name":"terminal","tool":"terminal","cwd":"/tmp","argv":[],"lane_uuid":"lane-0"}`)
	case EventProviderBound:
		return json.RawMessage(`{"provider_uuid":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","argv":["codex","resume","aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"]}`)
	case EventActivity:
		if choice%2 == 0 {
			return json.RawMessage(`{"source":"human_input"}`)
		}
		return json.RawMessage(`{"source":"not-an-activity-source"}`)
	case EventRenamed:
		return json.RawMessage(`{"name":"renamed"}`)
	case EventReopened:
		return json.RawMessage(`{"newLaneId":"replacement"}`)
	case EventProviderRebound:
		return json.RawMessage(`{"provider_uuid":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","new_lane_id":"replacement"}`)
	default:
		return json.RawMessage(`{}`)
	}
}

func cloneEvents(events []Event) []Event {
	cloned := make([]Event, len(events))
	copy(cloned, events)
	for index := range cloned {
		cloned[index].Payload = append(json.RawMessage(nil), cloned[index].Payload...)
	}
	return cloned
}

func dataBit(data []byte, index int) bool {
	return len(data) != 0 && data[index%len(data)]&1 != 0
}
