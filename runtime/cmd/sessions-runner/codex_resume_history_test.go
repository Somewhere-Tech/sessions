package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestCodexResumeRestoresDisplayWithoutSendingInput(t *testing.T) {
	r := newCodexTestRunner(t)
	_ = r.historyFile.Close()
	r.cfg.configDir = t.TempDir()
	id := "11111111-2222-4333-8444-555555555555"
	r.cfg.args = []string{"resume", id}
	directory := filepath.Join(r.cfg.configDir, "sessions", "2026", "09", "29")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "rollout-2026-09-29-"+id+".jsonl")
	source := []byte(`{"type":"session_meta","payload":{"id":"` + id + `","cwd":"/tmp"}}
{"type":"response_item","timestamp":"2026-09-29T12:00:00Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Keep my original request"}]}}
{"type":"response_item","timestamp":"2026-09-29T12:00:01Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Original answer"}]}}
`)
	if err := os.WriteFile(path, source, 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.prepareResumeHistory(); err != nil {
		t.Fatal(err)
	}
	defer r.closeHistory()
	if len(r.history) != 2 || !bytes.Contains(r.history[1], []byte("Original answer")) {
		t.Fatalf("restored display = %s", r.history)
	}
	before, _ := os.ReadFile(r.paths.Structured)
	r.closeHistory()
	if err := r.prepareResumeHistory(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(r.paths.Structured)
	if !bytes.Equal(before, after) {
		t.Fatal("opening existing runtime duplicated history")
	}
	unchanged, _ := os.ReadFile(path)
	if !bytes.Equal(source, unchanged) {
		t.Fatal("provider transcript changed")
	}
	if len(r.turnClient.(*fakeCodexTurnClient).steered) != 0 {
		t.Fatal("restoring display submitted model input")
	}
}

func TestCodexResumeDisplayWindowIsBounded(t *testing.T) {
	r := newCodexTestRunner(t)
	r.closeHistory()
	r.cfg.configDir = t.TempDir()
	id := "11111111-2222-4333-8444-555555555555"
	r.cfg.args = []string{"resume", id}
	dir := filepath.Join(r.cfg.configDir, "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var transcript bytes.Buffer
	for index := 0; index < 2*structuredHistoryLimit; index++ {
		fmt.Fprintf(&transcript, `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer-%d"}]}}`+"\n", index)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-"+id+".jsonl"), transcript.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.prepareResumeHistory(); err != nil {
		t.Fatal(err)
	}
	defer r.closeHistory()
	if len(r.history) != structuredHistoryLimit {
		t.Fatalf("display window has %d events, want %d", len(r.history), structuredHistoryLimit)
	}
	if bytes.Contains(r.history[0], []byte(`"answer-0"`)) {
		t.Fatal("display restored the old head instead of the bounded recent tail")
	}
	var event map[string]any
	if json.Unmarshal(r.history[len(r.history)-1], &event) != nil || event["source"] != "sessions-continuation" {
		t.Fatal("restored answers must remain visible alongside live app-server events")
	}
}

func TestCodexResumeDisplayFailureDoesNotPreventRecovery(t *testing.T) {
	r := newCodexTestRunner(t)
	_ = r.historyFile.Close()
	r.cfg.configDir = t.TempDir()
	r.cfg.args = []string{"resume", "11111111-2222-4333-8444-555555555555"}
	if err := r.prepareResumeHistory(); err != nil {
		t.Fatalf("display failure stopped provider recovery: %v", err)
	}
	defer r.closeHistory()
	var event map[string]any
	if len(r.history) != 1 || json.Unmarshal(r.history[0], &event) != nil || event["subtype"] != "resume_history_unavailable" {
		t.Fatalf("missing instructional history error: %s", r.history)
	}
	if _, err := os.Stat(state.For(r.paths.Dir, r.paths.ID).Structured); err != nil {
		t.Fatal(err)
	}
}
