package watch

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func collectEvents(t *testing.T, events <-chan SessionEvent, count int, timeout time.Duration) []SessionEvent {
	t.Helper()
	collected := make([]SessionEvent, 0, count)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for len(collected) < count {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatalf("event channel closed after %d/%d events", len(collected), count)
			}
			collected = append(collected, event)
		case <-deadline.C:
			t.Fatalf("timed out after %d/%d events", len(collected), count)
		}
	}
	return collected
}

func assertNoEvent(t *testing.T, events <-chan SessionEvent, duration time.Duration) {
	t.Helper()
	select {
	case event := <-events:
		t.Fatalf("unexpected event: %#v", event)
	case <-time.After(duration):
	}
}

func assertEventsJSONEqual(t *testing.T, got, want []SessionEvent) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var gotDecoded any
	var wantDecoded any
	if err := json.Unmarshal(gotJSON, &gotDecoded); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wantJSON, &wantDecoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotDecoded, wantDecoded) {
		t.Fatalf("events = %s, want %s", gotJSON, wantJSON)
	}
}

func eventText(event SessionEvent) string {
	message, _ := event["message"].(map[string]any)
	content, _ := message["content"].([]any)
	text := ""
	for _, raw := range content {
		block, _ := raw.(map[string]any)
		if value, ok := block["text"].(string); ok {
			if text != "" {
				text += "\n"
			}
			text += value
		}
	}
	return text
}
