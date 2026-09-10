package watch

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeRollout lays down one Codex rollout: session_meta, one user turn, then
// trailerBytes of further records. Everything the resumable card needs is in
// the first two records; the trailer is what a long conversation adds after it.
func writeRollout(t *testing.T, dir, id, cwd string, trailerBytes int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(dir, "rollout-"+id+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	writer := bufio.NewWriterSize(file, 1<<20)
	const stamp = "2026-09-10T00:00:00Z"
	encode := func(value any) []byte {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return append(encoded, '\n')
	}
	if _, err := writer.Write(encode(map[string]any{
		"timestamp": stamp, "type": "session_meta",
		"payload": map[string]any{"id": id, "cwd": cwd, "timestamp": stamp, "originator": "codex_cli_rs"},
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(encode(map[string]any{
		"timestamp": stamp, "type": "response_item",
		"payload": map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "the opening request"}}},
	})); err != nil {
		t.Fatal(err)
	}
	filler := make([]byte, 400)
	for index := range filler {
		filler[index] = 'a' + byte(index%26)
	}
	trailer := encode(map[string]any{
		"timestamp": stamp, "type": "response_item",
		"payload": map[string]any{"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": string(filler)}}},
	})
	for written := 0; written < trailerBytes; written += len(trailer) {
		if _, err := writer.Write(trailer); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// scanCost measures one isolated scan. The Claude root is an empty directory
// that exists, never "": an empty Claude root makes the scanner fall back to
// this machine's own ~/.claude, and a measurement must not read the user's
// conversations.
func scanCost(t *testing.T, claudeDir, codexDir string, runs int) (uint64, []ResumableSession) {
	t.Helper()
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	scanned := ScanResumableConversationsIn(claudeDir, codexDir)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range runs {
		scanned = ScanResumableConversationsIn(claudeDir, codexDir)
	}
	runtime.ReadMemStats(&after)
	return (after.TotalAlloc - before.TotalAlloc) / uint64(runs), scanned
}

// Listing conversations is what a person does constantly; opening one is what
// they do occasionally. The card a listing builds is finished once the rollout
// has named itself and shown its first request, so the rest of the read budget
// must not be decoded on the way past. Both rollouts here are well inside that
// budget and differ only in how much follows the opening exchange.
func TestCodexScanStopsOnceTheCardIsComplete(t *testing.T) {
	const (
		shortID = "11111111-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		longID  = "22222222-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
		runs    = 20
	)
	shortRoot, longRoot := t.TempDir(), t.TempDir()
	shortDir := filepath.Join(shortRoot, "sessions", "2026", "09", "10")
	longDir := filepath.Join(longRoot, "sessions", "2026", "09", "10")
	writeRollout(t, shortDir, shortID, filepath.Join(shortRoot, "project"), 8*1024)
	writeRollout(t, longDir, longID, filepath.Join(longRoot, "project"), 480*1024)

	shortCost, shortScanned := scanCost(t, filepath.Join(shortRoot, "claude"), filepath.Join(shortRoot, "sessions"), runs)
	longCost, longScanned := scanCost(t, filepath.Join(longRoot, "claude"), filepath.Join(longRoot, "sessions"), runs)
	t.Logf("8 KiB trailer: %d alloc-bytes/scan; 480 KiB trailer: %d alloc-bytes/scan", shortCost, longCost)

	// The card is the same either way, down to the opening request it shows.
	for name, scanned := range map[string][]ResumableSession{"short": shortScanned, "long": longScanned} {
		if len(scanned) != 1 {
			t.Fatalf("%s scan returned %d conversations", name, len(scanned))
		}
		session := scanned[0]
		if session.Tool != "codex" || session.FirstUserMessage != "the opening request" || session.Cwd == "" {
			t.Fatalf("%s card = %#v", name, session)
		}
	}
	if shortScanned[0].SessionID != shortID || longScanned[0].SessionID != longID {
		t.Fatalf("scan reported %q and %q", shortScanned[0].SessionID, longScanned[0].SessionID)
	}

	// Sixty times the trailing conversation, and the listing must not pay for
	// it. The bound is loose on purpose: this guards against decoding the whole
	// read budget again, not a particular allocation figure.
	if longCost > shortCost*2 {
		t.Fatalf("scanning cost followed the conversation length: %d bytes for an 8 KiB trailer, "+
			"%d bytes for a 480 KiB one", shortCost, longCost)
	}
}
