package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
)

func TestRecoveryAndPickerOutputStripTerminalControls(t *testing.T) {
	injected := "hello\x1b]52;c;clipboard\a\rspoof"
	var output bytes.Buffer
	a := &app{stdout: &output, stderr: &output}
	report := recovery.Report{Lanes: []recovery.Lane{{
		ID: "lost", Name: injected, Cwd: injected, Tool: injected, Class: ledger.ClassUnexpectedlyLost,
	}}}
	if err := writeRecoveryPlan(a, report, true); err != nil {
		t.Fatal(err)
	}
	picker := &conversationPicker{
		app: a, rows: []conversationRow{{Name: injected, CWD: injected, Resume: []string{"codex", injected}}},
		previews: map[int][]conversationPreviewMessage{0: {{Role: injected, Text: injected}}},
	}
	if err := picker.showPreview(0); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(output.String(), "\x1b\a\r") || !strings.Contains(output.String(), "hello") {
		t.Fatalf("unsafe or missing output: %q", output.String())
	}
	if picker.previews[0][0].Text != injected {
		t.Fatal("sanitization changed stored transcript")
	}
}
