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

// doctorMirrorFixture stores four mirrors in one scratch runner state
// directory: intact, capped, write-failing, and one whose sidecar is gone.
func doctorMirrorFixture(t *testing.T) map[string]string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := filepath.Join(home, "runners")
	t.Setenv("SESSIONS_STATE_DIR", stateDir)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	ids := map[string]string{
		"intact":    "1c9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4bc1",
		"capped":    "2c9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4bc2",
		"failing":   "3c9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4bc3",
		"noSidecar": "4c9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4bc4",
	}
	for _, id := range ids {
		if err := os.WriteFile(watch.TranscriptMirrorPath(stateDir, id),
			[]byte(`{"uuid":"a","message":"kept"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for state, meta := range map[string]watch.TranscriptMirrorMeta{
		"intact": {Version: 1, Records: 1, Bytes: 30},
		"capped": {Version: 1, Records: 4000, Bytes: 1 << 20, Capped: true, CapBytes: watch.DefaultTranscriptMirrorCapBytes},
		"failing": {Version: 1, Records: 12, Bytes: 400,
			WriteErrors: 5, LastError: "write mirror: permission denied"},
	} {
		payload, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		path := watch.TranscriptMirrorMetaPath(watch.TranscriptMirrorPath(stateDir, ids[state]))
		if err := os.WriteFile(path, append(payload, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

// doctor is where someone goes when something feels wrong, and a conversation
// that came back shorter than they remember is one of the few faults that shows
// up nowhere else: no session looks unhealthy, because the session is fine.
func TestDoctorFindsStoredConversationsThatStoppedRecording(t *testing.T) {
	ids := doctorMirrorFixture(t)

	damaged := damagedTranscriptMirrors()
	if len(damaged) != 2 {
		t.Fatalf("damaged = %+v, want exactly the capped and write-failing conversations", damaged)
	}
	found := make(map[string]string, len(damaged))
	for _, row := range damaged {
		found[row.ID] = row.Detail
	}
	if _, ok := found[ids["capped"]]; !ok {
		t.Fatalf("the capped conversation was not reported: %+v", damaged)
	}
	if _, ok := found[ids["failing"]]; !ok {
		t.Fatalf("the write-failing conversation was not reported: %+v", damaged)
	}
	// An intact mirror and a mirror of unknown health are both left out, for
	// opposite reasons that must not be collapsed: one is known good, the other
	// is not known at all, and neither is damage.
	if _, ok := found[ids["intact"]]; ok {
		t.Fatal("an intact conversation was reported as damaged")
	}
	if _, ok := found[ids["noSidecar"]]; ok {
		t.Fatal("a conversation whose health is unknown was reported as damaged")
	}

	if !strings.Contains(found[ids["capped"]], "4096 MB") {
		t.Fatalf("capped detail = %q, want the size limit named", found[ids["capped"]])
	}
	if !strings.Contains(found[ids["failing"]], "5 writes") ||
		!strings.Contains(found[ids["failing"]], "permission denied") {
		t.Fatalf("write-failure detail = %q, want the count and the cause", found[ids["failing"]])
	}
}

// The report has to name the conversations and give advice that is actually
// available. doctor's usual answer -- recreate the session -- cannot restore a
// conversation, so saying it here would waste the one chance the user has.
func TestDoctorMirrorReportSaysWhatCanStillBeDone(t *testing.T) {
	ids := doctorMirrorFixture(t)

	var stdout bytes.Buffer
	application := &app{stdout: &stdout}
	application.writeDoctorMirrorHealth(damagedTranscriptMirrors())
	report := stdout.String()

	for _, state := range []string{"capped", "failing"} {
		if !strings.Contains(report, prefixString(ids[state], 8)) {
			t.Fatalf("the %s conversation is not named:\n%s", state, report)
		}
	}
	for _, phrase := range []string{
		"missing records",
		"cannot be repaired",
		"sessions source <id>",
		"full or unwritable",
	} {
		if !strings.Contains(report, phrase) {
			t.Fatalf("the report is missing %q:\n%s", phrase, report)
		}
	}
	// The loss is reported, and it does not move the exit status. Doctor's
	// non-zero means sessions need recreate, which recreating them clears. A
	// copy that stopped recording can never be cleared -- it is append-only,
	// and rescuing the provider's transcript does not change what Sessions
	// holds -- so failing here would leave doctor permanently red on any host
	// that has ever lost records, with nothing that turns it green. That is how
	// a health check stops being read.
	if application.exitCode != 0 {
		t.Fatalf("exitCode = %d, want the report without a permanent failure "+
			"for a condition no action can clear", application.exitCode)
	}
	if strings.Contains(report, "recreate") {
		t.Fatalf("the report offers a recreate, which cannot restore a conversation:\n%s", report)
	}
}

// Silence when nothing is wrong, and silence that costs no exit code. A health
// report that always speaks is one users learn to skip.
func TestDoctorSaysNothingWhenNoConversationReportsAFault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := filepath.Join(home, "runners")
	t.Setenv("SESSIONS_STATE_DIR", stateDir)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "5c9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4bc5"
	mirror, err := watch.OpenTranscriptMirror(watch.TranscriptMirrorOptions{
		Path: watch.TranscriptMirrorPath(stateDir, id), SessionID: id,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mirror.Append([]byte(`{"uuid":"a","message":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	if err := mirror.Close(); err != nil {
		t.Fatal(err)
	}

	if damaged := damagedTranscriptMirrors(); len(damaged) != 0 {
		t.Fatalf("damaged = %+v, want nothing reported for an intact conversation", damaged)
	}
	var stdout bytes.Buffer
	application := &app{stdout: &stdout}
	application.writeDoctorMirrorHealth(nil)
	if stdout.Len() != 0 || application.exitCode != 0 {
		t.Fatalf("output = %q exitCode = %d, want silence", stdout.String(), application.exitCode)
	}
}
