package api

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

// `source_kind: sessions-mirror` already says the provider's file is gone. It
// does not say whether the copy that replaced it is the whole conversation, and
// reading a conversation back is exactly when the reader needs telling. Without
// this the endpoint described a mirror missing half its records identically to
// one missing none.
func TestHistorySourceReportsWhenSessionsOwnCopyStoppedRecording(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	daemon := newTestDaemon(t)

	intactID := "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	damagedID := "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb"
	unknownID := "cccccccc-3333-4333-8333-cccccccccccc"

	for _, id := range []string{intactID, damagedID, unknownID} {
		writeClaudeHistoryFixture(t, daemon, home, id, "mirrored recall", nil)
		mirrorPath := watch.TranscriptMirrorPath(daemon.config.RunnerStateDir, id)
		if err := os.WriteFile(mirrorPath, claudeTranscriptLines("mirrored question"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeMirrorSidecar(t, watch.TranscriptMirrorPath(daemon.config.RunnerStateDir, intactID),
		watch.TranscriptMirrorMeta{Version: 1, Records: 1, Bytes: 60})
	writeMirrorSidecar(t, watch.TranscriptMirrorPath(daemon.config.RunnerStateDir, damagedID),
		watch.TranscriptMirrorMeta{
			Version: 1, Records: 3, Bytes: 200, Capped: true,
			CapBytes: watch.DefaultTranscriptMirrorCapBytes,
		})
	// unknownID keeps no sidecar at all.

	for _, expected := range []struct {
		id      string
		damaged bool
		phrase  string
	}{
		{id: intactID, damaged: false},
		{id: damagedID, damaged: true, phrase: "512 MB"},
		// Unknown health is not damage. A mirror written before the sidecar
		// existed, or one whose sidecar was lost, is not thereby incomplete.
		{id: unknownID, damaged: false},
	} {
		response := serve(t, daemon.handler, http.MethodGet, "/api/history/"+expected.id+"/source",
			nil, "127.0.0.1:4321", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("source(%s) status=%d body=%s", expected.id, response.Code, response.Body.String())
		}
		var source integrations.HistorySource
		decodeBody(t, response, &source)
		if source.SourceKind != string(watch.ClaudeMirror) {
			t.Fatalf("source_kind(%s) = %q, want the mirror kind so the health check is reached",
				expected.id, source.SourceKind)
		}
		if source.MirrorDamaged != expected.damaged {
			t.Fatalf("mirror_damaged(%s) = %v, want %v", expected.id, source.MirrorDamaged, expected.damaged)
		}
		if expected.damaged && !strings.Contains(source.MirrorDetail, expected.phrase) {
			t.Fatalf("mirror_detail(%s) = %q, want it to contain %q",
				expected.id, source.MirrorDetail, expected.phrase)
		}
		if !expected.damaged && source.MirrorDetail != "" {
			t.Fatalf("mirror_detail(%s) = %q, want nothing claimed about a copy with no known fault",
				expected.id, source.MirrorDetail)
		}
	}
}

func writeMirrorSidecar(t *testing.T, mirrorPath string, meta watch.TranscriptMirrorMeta) {
	t.Helper()
	payload, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(watch.TranscriptMirrorMetaPath(mirrorPath), append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}
