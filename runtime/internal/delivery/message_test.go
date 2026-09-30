package delivery

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompleteMessageMatchingPreservesInteriorText(t *testing.T) {
	intended := "BEGIN\r\n" + strings.Repeat("é🙂 word ", 1024) + "\r\nEND\n"
	for _, test := range []struct {
		name, text string
		want       bool
	}{
		{"complete", intended, true},
		{"normalized-newlines", strings.TrimSpace(strings.ReplaceAll(intended, "\r\n", "\n")), true},
		{"suffix", intended[len(intended)-310:], false},
		{"missing-first-character", intended[1:], false},
		{"missing-interior", strings.Replace(intended, "word", "", 1), false},
		{"different-unicode", strings.Replace(intended, "é", "e", 1), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, content := range []any{test.text, []any{map[string]any{"type": "text", "text": test.text}}} {
				raw, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}})
				if _, got := MatchingUserText(raw, PasteStart+intended+PasteEnd); got != test.want {
					t.Fatalf("match=%v want=%v", got, test.want)
				}
			}
		})
	}
	raw, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "text": intended}}}})
	if _, matches := MatchingUserText(raw, intended); matches {
		t.Fatal("tool result was treated as authored user input")
	}
}

func TestLegacyPasteWrapsOnceAndRefusesControlEscapes(t *testing.T) {
	for _, text := range []string{"one\ntwo", strings.Repeat("é🙂", 4096)} {
		want := PasteStart + text + PasteEnd
		for _, input := range []string{text, want} {
			got, err := LegacyPaste(input)
			if err != nil || got != want {
				t.Fatalf("paste bytes=%d want=%d err=%v", len(got), len(want), err)
			}
		}
	}
	for _, text := range []string{"hello\x03", "hello" + PasteEnd + "tail", PasteStart + "unfinished"} {
		if _, err := LegacyPaste(text); err == nil {
			t.Fatal("unrepresentable terminal control was accepted")
		}
	}
}
