package watch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProviderTranscript(t *testing.T, path string, records int) {
	t.Helper()
	var builder strings.Builder
	for index := range records {
		fmt.Fprintf(&builder,
			`{"type":"user","uuid":"u%d","sessionId":"s1","message":{"role":"user","content":"line %d"}}`+"\n",
			index, index)
	}
	if err := os.WriteFile(path, []byte(builder.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The conversations most at risk are the ones nobody is watching: an ended
// session whose provider transcript still exists is exactly what the retention
// timer deletes next.
func TestBackfillRescuesAnUnwatchedConversation(t *testing.T) {
	dir := t.TempDir()
	provider := filepath.Join(dir, "conversation.jsonl")
	writeProviderTranscript(t, provider, 5)
	mirrorPath := TranscriptMirrorPath(dir, "1b9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4bed")

	result, err := BackfillTranscriptMirror(provider, TranscriptMirrorOptions{
		Path: mirrorPath, SessionID: "1b9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4bed", Tool: "claude",
	})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if result.Copied != 5 || !result.Complete() {
		t.Fatalf("result = %+v, want 5 copied and complete", result)
	}

	// The provider file is a copy source, never a move source.
	if _, err := os.Stat(provider); err != nil {
		t.Fatalf("the provider transcript was disturbed: %v", err)
	}

	// Now the provider prunes it, which is the whole scenario.
	if err := os.Remove(provider); err != nil {
		t.Fatal(err)
	}
	if !TranscriptMirrorUsable(mirrorPath) {
		t.Fatal("the mirror is not usable after the provider transcript was pruned")
	}
	records, err := TranscriptMirrorRecords(mirrorPath)
	if err != nil {
		t.Fatalf("read mirror: %v", err)
	}
	if len(records) != 5 {
		t.Fatalf("mirror holds %d records, want the whole conversation", len(records))
	}
}

// Backfill is the kind of thing a user runs twice. It must not duplicate, and
// it must be able to pick up records added since the last run.
func TestBackfillIsIdempotentAndResumes(t *testing.T) {
	dir := t.TempDir()
	provider := filepath.Join(dir, "conversation.jsonl")
	writeProviderTranscript(t, provider, 3)
	mirrorPath := TranscriptMirrorPath(dir, "2b9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4bed")
	options := TranscriptMirrorOptions{Path: mirrorPath, SessionID: "2b9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4bed"}

	first, err := BackfillTranscriptMirror(provider, options)
	if err != nil {
		t.Fatal(err)
	}
	if first.Copied != 3 {
		t.Fatalf("first run copied %d, want 3", first.Copied)
	}

	second, err := BackfillTranscriptMirror(provider, options)
	if err != nil {
		t.Fatal(err)
	}
	if second.Copied != 0 || second.AlreadyPresent != 3 {
		t.Fatalf("second run = %+v, want nothing copied and 3 already present", second)
	}

	// The conversation continued after the first backfill.
	writeProviderTranscript(t, provider, 6)
	third, err := BackfillTranscriptMirror(provider, options)
	if err != nil {
		t.Fatal(err)
	}
	if third.Copied != 3 || third.AlreadyPresent != 3 {
		t.Fatalf("third run = %+v, want the 3 new records copied", third)
	}
	records, err := TranscriptMirrorRecords(mirrorPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 6 {
		t.Fatalf("mirror holds %d records, want 6 with no duplicates", len(records))
	}
}

// A provider file that is already gone is the common case on this machine --
// most recorded conversations no longer have one. That is nothing the caller
// can act on, so it is not an error.
func TestBackfillOfAPrunedTranscriptIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	result, err := BackfillTranscriptMirror(filepath.Join(dir, "gone.jsonl"), TranscriptMirrorOptions{
		Path: TranscriptMirrorPath(dir, "3b9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4bed"),
	})
	if err != nil {
		t.Fatalf("a missing provider transcript reported an error: %v", err)
	}
	if result.Copied != 0 || !result.Complete() {
		t.Fatalf("result = %+v, want an empty complete result", result)
	}
}

// A conversation containing one oversized record must still be rescued in
// part, with the loss reported rather than swallowed.
func TestBackfillReportsWhatItCouldNotStore(t *testing.T) {
	dir := t.TempDir()
	provider := filepath.Join(dir, "conversation.jsonl")
	good := `{"type":"user","uuid":"u0","sessionId":"s1","message":{"role":"user","content":"keep me"}}`
	huge := `{"type":"user","uuid":"u1","sessionId":"s1","message":{"role":"user","content":"` +
		strings.Repeat("x", transcriptScanLineCap+16) + `"}}`
	if err := os.WriteFile(provider, []byte(good+"\n"+huge+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mirrorPath := TranscriptMirrorPath(dir, "4b9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4bed")

	result, err := BackfillTranscriptMirror(provider, TranscriptMirrorOptions{
		Path: mirrorPath, SessionID: "4b9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4bed",
	})
	if err == nil {
		t.Fatal("an unreadable record was not reported")
	}
	if result.Copied != 1 {
		t.Fatalf("result = %+v, want the readable record kept", result)
	}
	if result.Complete() {
		t.Fatal("a lossy backfill reported itself complete")
	}
	records, readErr := TranscriptMirrorRecords(mirrorPath)
	if readErr != nil || len(records) != 1 {
		t.Fatalf("mirror = %d records (%v), want the one record that fit", len(records), readErr)
	}
}

// codexRolloutLines is a rollout fixture shaped like the real thing: no uuid
// field on any record, and a byte-identical repeat. Both properties are taken
// from ~/.codex/sessions on the development machine, where 0 of 49,871 scanned
// records carried a uuid and 4 of 175 rollouts contained byte-identical
// duplicate lines -- one of them 554 of them in a single 7,743-line file.
func codexRolloutLines() []string {
	return []string{
		`{"timestamp":"2026-08-05T11:50:32.000Z","type":"session_meta","payload":{"session_id":"019fd343-5368-7d40-92ac-3ff75509ab9a","cwd":"/Users/uzair/pretty-PTY"}}`,
		`{"timestamp":"2026-08-05T11:50:33.000Z","type":"turn_context","payload":{"model":"gpt-5"}}`,
		`{"timestamp":"2026-08-05T11:50:34.000Z","type":"response_item","payload":{"type":"message","role":"user"}}`,
		// The repeat. Byte identical to the turn_context above.
		`{"timestamp":"2026-08-05T11:50:33.000Z","type":"turn_context","payload":{"model":"gpt-5"}}`,
		`{"timestamp":"2026-08-05T12:10:00.000Z","type":"compacted","payload":{}}`,
		`{"timestamp":"2026-08-05T12:10:00.000Z","type":"event_msg","payload":{"type":"context_compacted"}}`,
	}
}

// TestBackfillMirrorsCodexRolloutLosslessly is the evidence behind the decision
// recorded at the top of codex_watcher.go: Codex is not mirrored by teeing the
// watcher, but IS mirrorable through the provider-neutral backfill path. That
// claim is only true if the mirror is lossless on rollout-shaped data, which it
// was not before record identity became a multiset -- every repeated line was
// dropped, silently, because Codex records carry no uuid to key on.
func TestBackfillMirrorsCodexRolloutLosslessly(t *testing.T) {
	dir := t.TempDir()
	rollout := filepath.Join(dir, "rollout-2026-08-05T11-50-32-019fd343-5368-7d40-92ac-3ff75509ab9a.jsonl")
	lines := codexRolloutLines()
	if err := os.WriteFile(rollout, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mirrorPath := TranscriptMirrorPath(dir, "sess-codex")

	result, err := BackfillTranscriptMirror(rollout, TranscriptMirrorOptions{
		Path: mirrorPath, SessionID: "sess-codex", Tool: "codex",
		ProviderSessionID: "019fd343-5368-7d40-92ac-3ff75509ab9a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Complete() {
		t.Fatalf("result = %+v, want a complete backfill", result)
	}
	if result.Copied != len(lines) {
		t.Fatalf("copied %d records, want all %d including the repeat", result.Copied, len(lines))
	}

	// Byte-for-byte identity, in order. The mirror must be a legal rollout so
	// every existing reader works on it by substituting the path.
	stored, err := os.ReadFile(mirrorPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(stored), strings.Join(lines, "\n")+"\n"; got != want {
		t.Fatalf("mirror is not a byte-identical copy:\n got %q\nwant %q", got, want)
	}

	// Re-running is the intended usage and must add nothing.
	repeat, err := BackfillTranscriptMirror(rollout, TranscriptMirrorOptions{
		Path: mirrorPath, SessionID: "sess-codex", Tool: "codex",
		ProviderSessionID: "019fd343-5368-7d40-92ac-3ff75509ab9a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if repeat.Copied != 0 || repeat.AlreadyPresent != len(lines) {
		t.Fatalf("repeat backfill = %+v, want everything already present", repeat)
	}
}

// TestBackfillKeepsRepeatedClaudeStateRecords is the same loss on the Claude
// side, where it is worse because these records drive resume state. Native
// `claude --resume` reads permission mode back last-one-wins, so collapsing the
// repeats does not shrink the transcript, it rewinds the restored mode.
func TestBackfillKeepsRepeatedClaudeStateRecords(t *testing.T) {
	dir := t.TempDir()
	provider := filepath.Join(dir, "provider.jsonl")
	lines := []string{
		`{"type":"user","uuid":"u0","message":{"role":"user","content":"go"}}`,
		`{"type":"permission-mode","permissionMode":"bypassPermissions","sessionId":"s"}`,
		`{"type":"permission-mode","permissionMode":"normal","sessionId":"s"}`,
		`{"type":"permission-mode","permissionMode":"bypassPermissions","sessionId":"s"}`,
	}
	if err := os.WriteFile(provider, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mirrorPath := TranscriptMirrorPath(dir, "sess-state")

	result, err := BackfillTranscriptMirror(provider, TranscriptMirrorOptions{Path: mirrorPath})
	if err != nil {
		t.Fatal(err)
	}
	if result.Copied != len(lines) {
		t.Fatalf("copied %d, want %d", result.Copied, len(lines))
	}
	records, err := TranscriptMirrorRecords(mirrorPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := records[len(records)-1]["permissionMode"]; got != "bypassPermissions" {
		t.Fatalf("restored permission mode = %v, want bypassPermissions", got)
	}
}
