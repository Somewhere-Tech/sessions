package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

// mirrorHealthFixture builds a scratch machine holding one Claude session per
// mirror health state, and returns the state directory. Every session's
// provider transcript is absent, so each one reaches the "Sessions already has
// a copy" branch and differs only in what its copy says about itself.
func mirrorHealthFixture(t *testing.T) map[string]string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := filepath.Join(home, "runners")
	t.Setenv("SESSIONS_STATE_DIR", stateDir)
	if err := os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	ids := map[string]string{
		"healthy":     "1a9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4be1",
		"capped":      "2a9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4be2",
		"writeErrors": "3a9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4be3",
		"noSidecar":   "4a9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4be4",
		"corrupt":     "5a9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4be5",
	}
	for state, id := range ids {
		metadata := `{"id":"` + id + `","cmd":"claude","cwd":"` + filepath.Join(home, "work-"+state) +
			`","createdAt":1,"cols":80,"rows":24}`
		if err := os.WriteFile(filepath.Join(stateDir, id+".json"), []byte(metadata), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// The capped mirror is built by the production write path, so the sidecar
	// under test is one Sessions itself wrote.
	cappedPath := watch.TranscriptMirrorPath(stateDir, ids["capped"])
	capped, err := watch.OpenTranscriptMirror(watch.TranscriptMirrorOptions{
		Path: cappedPath, SessionID: ids["capped"], CapBytes: 120,
	})
	if err != nil {
		t.Fatal(err)
	}
	for index := range 20 {
		if _, err := capped.Append([]byte(`{"uuid":"c` + string(rune('a'+index)) + `","message":"long conversation"}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := capped.Close(); err != nil {
		t.Fatal(err)
	}
	if !watch.ReadTranscriptMirrorHealth(cappedPath).Capped {
		t.Fatal("the fixture failed to produce a genuinely capped mirror")
	}

	// Every other mirror holds the same readable conversation, so any
	// difference in what the command says comes from the sidecar alone.
	for _, state := range []string{"healthy", "writeErrors", "noSidecar", "corrupt"} {
		path := watch.TranscriptMirrorPath(stateDir, ids[state])
		if err := os.WriteFile(path, []byte(`{"uuid":"a","message":"kept"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeSidecar(t, watch.TranscriptMirrorPath(stateDir, ids["healthy"]), watch.TranscriptMirrorMeta{
		Version: 1, SessionID: ids["healthy"], Records: 1, Bytes: 30,
	})
	writeSidecar(t, watch.TranscriptMirrorPath(stateDir, ids["writeErrors"]), watch.TranscriptMirrorMeta{
		Version: 1, SessionID: ids["writeErrors"], Records: 1, Bytes: 30,
		WriteErrors: 7, LastError: "write mirror: no space left on device",
	})
	if err := os.WriteFile(
		watch.TranscriptMirrorMetaPath(watch.TranscriptMirrorPath(stateDir, ids["corrupt"])),
		[]byte("not json at all {{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	return ids
}

func writeSidecar(t *testing.T, mirrorPath string, meta watch.TranscriptMirrorMeta) {
	t.Helper()
	payload, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(watch.TranscriptMirrorMetaPath(mirrorPath), append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The defect: a capped mirror and a mirror whose every append failed were both
// reported with the same word as an intact one, so "Sessions has your
// conversation" was said about conversations Sessions has only part of.
func TestTranscriptsDistinguishesADamagedCopyFromAKeptOne(t *testing.T) {
	ids := mirrorHealthFixture(t)

	result, stderr, code := runTranscripts(t)
	if code != exitSatisfied {
		t.Fatalf("exit = %d stderr = %q", code, stderr)
	}
	if result.Examined != 5 {
		t.Fatalf("examined = %d, want all five conversations", result.Examined)
	}

	status := make(map[string]string, len(result.Sessions))
	detail := make(map[string]string, len(result.Sessions))
	for _, report := range result.Sessions {
		status[report.Session] = report.Status
		detail[report.Session] = report.Detail
	}
	for state, wanted := range map[string]string{
		"healthy":     transcriptStatusAlreadyKept,
		"capped":      transcriptStatusKeptDamaged,
		"writeErrors": transcriptStatusKeptDamaged,
		// Unknown health is not damage. A mirror whose sidecar is gone or
		// unreadable still holds whatever it holds, and calling it damaged
		// would make the warning meaningless for the mirrors that are.
		"noSidecar": transcriptStatusAlreadyKept,
		"corrupt":   transcriptStatusAlreadyKept,
	} {
		if got := status[ids[state]]; got != wanted {
			t.Fatalf("%s mirror reported %q, want %q", state, got, wanted)
		}
	}
	if result.AlreadyKept != 3 || result.KeptDamaged != 2 {
		t.Fatalf("already_kept = %d kept_damaged = %d, want 3 and 2", result.AlreadyKept, result.KeptDamaged)
	}
	// A status word alone leaves the reader unable to act. Each damaged copy
	// has to say which fault it hit.
	if !strings.Contains(detail[ids["capped"]], "size limit") {
		t.Fatalf("capped detail = %q, want the cap named", detail[ids["capped"]])
	}
	if !strings.Contains(detail[ids["writeErrors"]], "7 writes") ||
		!strings.Contains(detail[ids["writeErrors"]], "no space left on device") {
		t.Fatalf("write-error detail = %q, want the count and the cause", detail[ids["writeErrors"]])
	}
	if detail[ids["noSidecar"]] != "" || detail[ids["corrupt"]] != "" {
		t.Fatal("a mirror with unknown health was given a fault it has no evidence for")
	}
}

// The closing summary is the sentence a user actually reads, and it is the one
// that promised survival. It must count damaged copies apart from kept ones,
// name them, and say what is still possible.
func TestTranscriptsTextNamesDamagedCopiesAndWhatToDo(t *testing.T) {
	ids := mirrorHealthFixture(t)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"transcripts"}, strings.NewReader(""), &stdout, &stderr); code != exitSatisfied {
		t.Fatalf("exit = %d stderr = %q", code, stderr.String())
	}
	report := stdout.String()

	if !strings.Contains(report, "2 kept but damaged") {
		t.Fatalf("the summary does not count damaged copies apart from kept ones:\n%s", report)
	}
	// Three, not five: the promise of survival may only be made about the
	// copies that actually survived whole.
	if !strings.Contains(report, "3 conversations have a Sessions copy and survive") {
		t.Fatalf("damaged copies are still counted as having survived:\n%s", report)
	}
	for _, state := range []string{"capped", "writeErrors"} {
		if !strings.Contains(report, shortID(ids[state])) {
			t.Fatalf("the %s conversation is not named, so the user cannot act on it:\n%s", state, report)
		}
	}
	for _, phrase := range []string{
		"kept but damaged",
		"stopped recording",
		"Running this again cannot mend it",
		"sessions source <id>",
		"full or unwritable",
	} {
		if !strings.Contains(report, phrase) {
			t.Fatalf("the damaged summary is missing %q:\n%s", phrase, report)
		}
	}
	// Mirrors of unknown health must not appear in the damaged list at all.
	for _, state := range []string{"noSidecar", "corrupt"} {
		if strings.Contains(report, shortID(ids[state])) {
			t.Fatalf("a mirror of unknown health was reported as damaged:\n%s", report)
		}
	}
}

// With nothing wrong, nothing is said. A health report that always speaks is
// one users learn to skip.
func TestTranscriptsStaysSilentWhenEveryCopyIsIntact(t *testing.T) {
	stateDir, kept, _ := transcriptFixture(t)
	if _, stderr, code := runTranscripts(t, "--apply"); code != exitSatisfied {
		t.Fatalf("apply exit = %d stderr = %q", code, stderr)
	}
	if !watch.TranscriptMirrorUsable(watch.TranscriptMirrorPath(stateDir, kept)) {
		t.Fatal("the fixture did not produce a mirror to check")
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"transcripts"}, strings.NewReader(""), &stdout, &stderr); code != exitSatisfied {
		t.Fatalf("exit = %d stderr = %q", code, stderr.String())
	}
	report := stdout.String()
	if strings.Contains(report, "damaged") || strings.Contains(report, "stopped recording") {
		t.Fatalf("an intact copy was reported as damaged:\n%s", report)
	}
}
