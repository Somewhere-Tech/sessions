package watch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadAnchorDetectsInPlaceRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	original := []byte(strings.Repeat("a", 400) + "\n" + strings.Repeat("b", 400) + "\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	var anchor readAnchor
	anchor.advance(original)
	offset := int64(len(original))
	if !anchor.intact(file, offset) {
		t.Fatal("an untouched file must read as intact")
	}

	// The hazard: a rewrite in place that leaves the file LARGER, on the same
	// inode. Every stat-derived signal still says "same file, only grown".
	rewritten := []byte(strings.Repeat("c", 400) + "\n" + strings.Repeat("d", 400) + "\n" +
		strings.Repeat("e", 400) + "\n")
	if err := os.WriteFile(path, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
	if len(rewritten) <= len(original) {
		t.Fatal("fixture must grow the file for this to be the case under test")
	}
	rewrittenFile, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rewrittenFile.Close()
	if anchor.intact(rewrittenFile, offset) {
		t.Fatal("an in-place rewrite at a larger size must not read as intact")
	}
}

func TestReadAnchorVacuousAtOffsetZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var anchor readAnchor
	if !anchor.intact(file, 0) {
		t.Fatal("offset zero has nothing to contradict and must be intact")
	}
	anchor.advance([]byte("{}\n"))
	if !anchor.intact(file, 3) {
		t.Fatal("matching bytes must be intact")
	}
}

// TestReadAnchorRejectsShorterPrefix covers a file whose content before the
// offset is now shorter than the anchor. Those bytes cannot be the ones the
// anchor came from, whatever the total size says.
func TestReadAnchorRejectsShorterPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var anchor readAnchor
	anchor.advance([]byte(strings.Repeat("x", 100)))
	if anchor.intact(file, 50) {
		t.Fatal("a 100-byte anchor cannot fit before offset 50")
	}
}

// TestReadAnchorAccumulatesSmallChunks covers a transcript that arrives one
// short record at a time, which is the normal live case. The anchor has to
// widen to the full window rather than shrinking to the last chunk.
func TestReadAnchorAccumulatesSmallChunks(t *testing.T) {
	var anchor readAnchor
	for range 200 {
		anchor.advance([]byte("0123456789"))
	}
	if len(anchor.bytes) != claudeAnchorBytes {
		t.Fatalf("anchor width = %d, want %d", len(anchor.bytes), claudeAnchorBytes)
	}
	// And a single oversized chunk replaces it with that chunk's tail.
	anchor.advance([]byte(strings.Repeat("z", claudeAnchorBytes+64)))
	if len(anchor.bytes) != claudeAnchorBytes {
		t.Fatalf("anchor width after large chunk = %d", len(anchor.bytes))
	}
	if strings.Trim(string(anchor.bytes), "z") != "" {
		t.Fatal("a chunk wider than the window should fully replace the anchor")
	}
}

// TestClaudeWatcherRecoversFromInPlaceRewrite is the end-to-end form. The
// provider rewrites its transcript on the same inode to a larger size, which
// the previous identity-and-size check could not see. Without the anchor the
// tail resumes mid-file and every record the rewrite placed before the old
// offset is never read.
func TestClaudeWatcherRecoversFromInPlaceRewrite(t *testing.T) {
	projectDir := t.TempDir()
	stateDir := t.TempDir()
	const providerID = "eeeeeeee-1111-2222-3333-444444444444"
	providerPath := filepath.Join(projectDir, providerID+".jsonl")

	writeSessionEvents(t, providerPath, conversationEvents("first turn"), false)
	before, err := os.Stat(providerPath)
	if err != nil {
		t.Fatal(err)
	}

	mirrorPath := TranscriptMirrorPath(stateDir, "sess-rewrite")
	watcher, err := WatchClaudeSession(ClaudeWatcherOptions{
		ClaudeSessionID: providerID, ProjectDir: projectDir,
		SessionID: "sess-rewrite", MirrorPath: mirrorPath,
		InitialDelay: time.Millisecond, PollInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	collectEvents(t, watcher.Events, 1, 2*time.Second)

	// Rewrite in place: same inode, strictly larger, entirely different
	// records. Writing through the existing file rather than replacing it is
	// what keeps the inode, so os.SameFile still says "same file".
	file, err := os.OpenFile(providerPath, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := append(conversationEvents("rescued turn one"), conversationEvents("rescued turn two")...)
	payload := sessionEventBytes(t, rewritten)
	if int64(len(payload)) <= before.Size() {
		t.Fatalf("fixture rewrite must be larger: %d <= %d", len(payload), before.Size())
	}
	if _, err := file.WriteAt(payload, 0); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := os.Stat(providerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("fixture must keep the same inode, or it is not the case under test")
	}
	if after.Size() < before.Size() {
		t.Fatal("fixture must not shrink, or the old size check would already catch it")
	}

	// Both rewritten turns must arrive. The first one sits before the stale
	// offset and is exactly what the old check skipped.
	events := collectEvents(t, watcher.Events, 2, 3*time.Second)
	seen := make(map[string]bool, len(events))
	for _, event := range events {
		seen[eventText(event)] = true
	}
	for _, want := range []string{"rescued turn one", "rescued turn two"} {
		if !seen[want] {
			t.Fatalf("missing %q after in-place rewrite; got %v", want, seen)
		}
	}

	// The mirror holds the union, and the rewrite is recorded as a generation
	// so the sidecar carries the evidence that the provider replaced content.
	if got := mirrorTexts(t, mirrorPath); len(got) != 3 {
		t.Fatalf("mirror texts = %v, want the original turn plus both rewritten ones", got)
	}
	if meta, ok := ReadTranscriptMirrorMeta(mirrorPath); !ok || meta.Generations == 0 {
		t.Fatalf("meta = %+v (ok=%v), want a recorded generation", meta, ok)
	}
}
