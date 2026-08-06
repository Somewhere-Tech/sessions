package watch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A mirror records that it stopped storing records. Until something reads that
// back, a conversation Sessions kept half of is indistinguishable from one it
// kept all of -- which is the exact failure a mirror exists to prevent.
//
// The capped mirror here is produced by the production write path, not by a
// hand-written sidecar: OpenTranscriptMirror with a small cap, then Append
// until it refuses.
func TestMirrorHealthReportsACapReachedByTheRealWritePath(t *testing.T) {
	mirrorPath := filepath.Join(t.TempDir(), "session"+TranscriptMirrorSuffix)
	mirror, err := OpenTranscriptMirror(TranscriptMirrorOptions{Path: mirrorPath, CapBytes: 120})
	if err != nil {
		t.Fatal(err)
	}
	stored := 0
	for index := range 20 {
		line := []byte(`{"uuid":"record-` + string(rune('a'+index)) + `","message":"conversation"}`)
		wrote, appendErr := mirror.Append(line)
		if appendErr != nil {
			t.Fatalf("append %d: %v", index, appendErr)
		}
		if wrote {
			stored++
		}
	}
	if err := mirror.Close(); err != nil {
		t.Fatal(err)
	}
	if stored == 0 || stored == 20 {
		t.Fatalf("stored %d of 20 records, want a genuinely capped mirror", stored)
	}

	health := ReadTranscriptMirrorHealth(mirrorPath)
	if !health.Known {
		t.Fatal("a mirror written by the production path has no readable sidecar")
	}
	if !health.Capped || !health.Degraded() {
		t.Fatalf("health = %+v, want a capped mirror reported as degraded", health)
	}
	if health.Records != stored {
		t.Fatalf("health.Records = %d, want the %d records actually stored", health.Records, stored)
	}
	detail := health.Detail()
	// The detail has to answer "what does this mean for my conversation", not
	// merely name a flag. A user who reads it and still does not know records
	// are missing has been told nothing.
	if !strings.Contains(detail, "size limit") || !strings.Contains(detail, "never copied") {
		t.Fatalf("detail = %q, want it to say the cap stopped it and records were not copied", detail)
	}
}

// A mirror that could not be written is a silent data-loss bug, so the count
// has to reach a reader. A genuine write failure needs a full or read-only
// filesystem, which a unit test cannot create portably, so the sidecar is
// written through the same struct and encoder writeMetaLocked uses.
func TestMirrorHealthReportsWriteFailures(t *testing.T) {
	mirrorPath := writeMirrorWithMeta(t, TranscriptMirrorMeta{
		Version: transcriptMirrorVersion, Records: 4, Bytes: 200,
		WriteErrors: 3, LastError: "write mirror: no space left on device",
	})

	health := ReadTranscriptMirrorHealth(mirrorPath)
	if !health.Known || !health.Degraded() || health.WriteErrors != 3 {
		t.Fatalf("health = %+v, want three recorded write failures reported as degraded", health)
	}
	detail := health.Detail()
	if !strings.Contains(detail, "3 writes") || !strings.Contains(detail, "missing") ||
		!strings.Contains(detail, "no space left on device") {
		t.Fatalf("detail = %q, want the count, the consequence, and the underlying error", detail)
	}
}

// A capped mirror that also failed writes must say both, or fixing one leaves
// the user believing the conversation is now whole.
func TestMirrorHealthReportsBothFaultsTogether(t *testing.T) {
	mirrorPath := writeMirrorWithMeta(t, TranscriptMirrorMeta{
		Version: transcriptMirrorVersion, Records: 9, Capped: true, CapBytes: DefaultTranscriptMirrorCapBytes,
		WriteErrors: 1, LastError: "input/output error",
	})

	detail := ReadTranscriptMirrorHealth(mirrorPath).Detail()
	if !strings.Contains(detail, "512 MB") || !strings.Contains(detail, "1 write") {
		t.Fatalf("detail = %q, want both the cap and the failed write named", detail)
	}
}

// Unknown health is not damage. Mirrors written before the sidecar existed, and
// sidecars lost to a crash, say nothing at all about the conversation stored
// beside them; reporting those as damaged would teach users to ignore the
// warning that matters. Reporting them as healthy would be a lie of the same
// size as the bug this file exists to close.
func TestMirrorHealthIsUnknownRatherThanEitherVerdict(t *testing.T) {
	for name, sidecar := range map[string]string{
		"missing":   "",
		"corrupt":   "not json at all {{{",
		"truncated": `{"version":1,"records":`,
		"empty":     "",
	} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			mirrorPath := filepath.Join(directory, "session"+TranscriptMirrorSuffix)
			if err := os.WriteFile(mirrorPath, []byte("{\"uuid\":\"a\"}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if name != "missing" {
				if err := os.WriteFile(TranscriptMirrorMetaPath(mirrorPath), []byte(sidecar), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			health := ReadTranscriptMirrorHealth(mirrorPath)
			if health.Known {
				t.Fatalf("%s sidecar reported known health: %+v", name, health)
			}
			if health.Degraded() {
				t.Fatalf("%s sidecar was reported as damaged, but nothing is known about it", name)
			}
			if health.Detail() != "" {
				t.Fatalf("%s sidecar produced a detail (%q) it has no evidence for", name, health.Detail())
			}
			// The conversation itself is untouched by any of this, and must
			// still be offered rather than withheld.
			if !TranscriptMirrorUsable(mirrorPath) {
				t.Fatalf("%s sidecar made a readable conversation unusable", name)
			}
		})
	}
}

// A healthy mirror must stay silent, or the warning is worthless.
func TestMirrorHealthSaysNothingAboutAnIntactMirror(t *testing.T) {
	mirrorPath := filepath.Join(t.TempDir(), "session"+TranscriptMirrorSuffix)
	mirror, err := OpenTranscriptMirror(TranscriptMirrorOptions{Path: mirrorPath, SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mirror.Append([]byte(`{"uuid":"a","message":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	if err := mirror.Close(); err != nil {
		t.Fatal(err)
	}

	health := ReadTranscriptMirrorHealth(mirrorPath)
	if !health.Known {
		t.Fatal("an intact mirror has no readable sidecar")
	}
	if health.Degraded() || health.Detail() != "" {
		t.Fatalf("health = %+v detail = %q, want an intact mirror reported as intact", health, health.Detail())
	}
}

// writeMirrorWithMeta stores a mirror and a sidecar encoded exactly the way
// writeMetaLocked encodes one.
func writeMirrorWithMeta(t *testing.T, meta TranscriptMirrorMeta) string {
	t.Helper()
	mirrorPath := filepath.Join(t.TempDir(), "session"+TranscriptMirrorSuffix)
	if err := os.WriteFile(mirrorPath, []byte("{\"uuid\":\"a\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(TranscriptMirrorMetaPath(mirrorPath), append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return mirrorPath
}
