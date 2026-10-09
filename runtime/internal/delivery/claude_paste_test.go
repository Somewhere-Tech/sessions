package delivery

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestClaudePastedMessageMatchesOnlyCompleteAuthoredContent(t *testing.T) {
	intended := "# Start now\r\n" + strings.Repeat("é🙂 preserve every word\n", 256) + "END"
	for _, test := range []struct {
		name, text string
		want       bool
	}{
		{"repeated-id", "\n\n<pasted_content id=\"7ab0\">\n" + intended + "\n</pasted_content id=\"7ab0\">\n", true},
		{"ordinary-close", "<pasted_content id=\"7ab0\">" + intended + "</pasted_content>", true},
		{"no-id", "<pasted_content>" + intended + "</pasted_content>", true},
		{"wrong-id", "<pasted_content id=\"7ab0\">" + intended + "</pasted_content id=\"other\">", false},
		{"partial", "<pasted_content id=\"7ab0\">" + intended, false},
		{"prefix", "another request\n<pasted_content>" + intended + "</pasted_content>", false},
		{"suffix", "<pasted_content>" + intended + "</pasted_content>\nextra", false},
		{"changed-body", "<pasted_content>" + strings.Replace(intended, "word", "", 1) + "</pasted_content>", false},
		{"body-suffix", "<pasted_content>END</pasted_content>", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, content := range []any{test.text, []any{map[string]any{"type": "text", "text": test.text}}} {
				raw, _ := json.Marshal(map[string]any{"type": "user", "timestamp": "2026-10-03T04:19:40.455Z",
					"message": map[string]any{"role": "user", "content": content}})
				if _, got := MatchingUserText(raw, PasteStart+intended+PasteEnd); got != test.want {
					t.Fatalf("match=%v want=%v", got, test.want)
				}
				at, _ := time.Parse(time.RFC3339Nano, "2026-10-03T04:19:40.455Z")
				if got := MatchesLateUserEvent(raw, MessageHash(intended), at.UnixMilli()); got != test.want {
					t.Fatalf("late match=%v want=%v", got, test.want)
				}
				if MatchesLateUserEvent(raw, MessageHash(intended), at.UnixMilli()+1) {
					t.Fatal("historical event accepted as a fresh delivery")
				}
			}
		})
	}
}

func TestClaudePastedMessageDoesNotChangeLiteralMarkupOrAcceptToolResults(t *testing.T) {
	text := "<pasted_content id=\"literal\">literal markup</pasted_content id=\"literal\">"
	raw, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}})
	if observed, ok := MatchingUserText(raw, text); !ok || observed != text {
		t.Fatal("literal user markup was rewritten")
	}
	raw, _ = json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user",
		"content": []any{map[string]any{"type": "tool_result", "text": text}}}})
	if _, ok := MatchingUserText(raw, "literal markup"); ok {
		t.Fatal("tool output counted as an authored paste")
	}
}

func TestEmptyClaudePasteNeverConfirmsDelivery(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"type": "user", "timestamp": "2026-10-03T04:19:40.455Z",
		"message": map[string]any{"role": "user", "content": "<pasted_content id=\"empty\">\n</pasted_content id=\"empty\">"}})
	if _, ok := MatchingUserText(raw, ""); ok || MatchesLateUserEvent(raw, MessageHash(""), 0) {
		t.Fatal("empty paste counted as delivery")
	}
}
