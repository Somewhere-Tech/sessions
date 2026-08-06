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

// transcriptFixture builds a scratch machine holding one Claude session whose
// provider transcript still exists, and one whose provider already pruned it.
func transcriptFixture(t *testing.T) (stateDir string, kept string, pruned string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir = filepath.Join(home, "runners")
	t.Setenv("SESSIONS_STATE_DIR", stateDir)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	kept = "6a9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4bed"
	pruned = "7a9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4bed"
	// Separate working directories, so each session has its own project
	// bucket. Sharing one would make the pruned session resolve to the other
	// session's transcript through the resolver's single-file fallback, which
	// is a different case: reported as unverified, and never copied.
	cwd := filepath.Join(home, "work")
	prunedCwd := filepath.Join(home, "other")

	for id, sessionCwd := range map[string]string{kept: cwd, pruned: prunedCwd} {
		metadata := `{"id":"` + id + `","cmd":"claude","cwd":"` + sessionCwd + `","createdAt":1,"cols":80,"rows":24}`
		if err := os.WriteFile(filepath.Join(stateDir, id+".json"), []byte(metadata), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	bucket := filepath.Join(home, ".claude", "projects", watch.EncodeClaudeCWD(cwd))
	if err := os.MkdirAll(bucket, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","uuid":"u1","sessionId":"` + kept +
		`","message":{"role":"user","content":"the conversation worth keeping"}}` + "\n"
	if err := os.WriteFile(filepath.Join(bucket, kept+".jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return stateDir, kept, pruned
}

func runTranscripts(t *testing.T, args ...string) (transcriptsResult, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"--json", "transcripts"}, args...), strings.NewReader(""), &stdout, &stderr)
	var result transcriptsResult
	if stdout.Len() > 0 {
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatalf("transcripts did not emit a decodable result: %v (%q)", err, stdout.String())
		}
	}
	return result, stderr.String(), code
}

// The default has to be a dry run: this sweeps the user's whole history, and
// they should see what it will touch before it touches anything.
func TestTranscriptsDryRunCopiesNothing(t *testing.T) {
	stateDir, kept, _ := transcriptFixture(t)

	result, stderr, code := runTranscripts(t)
	if code != exitSatisfied {
		t.Fatalf("exit = %d stderr = %q", code, stderr)
	}
	if result.Applied {
		t.Fatal("a dry run reported itself applied")
	}
	if result.Copyable != 1 || result.Copied != 0 {
		t.Fatalf("result = %+v, want one copyable and nothing copied", result)
	}
	if result.Unrecoverable != 1 {
		t.Fatalf("unrecoverable = %d, want the pruned conversation counted honestly", result.Unrecoverable)
	}
	if _, err := os.Stat(watch.TranscriptMirrorPath(stateDir, kept)); err == nil {
		t.Fatal("the dry run wrote a mirror")
	}
}

// Applying rescues what can still be read, leaves the provider's own file
// alone, and says plainly that the pruned conversation is beyond help.
func TestTranscriptsApplyRescuesWhatIsStillReadable(t *testing.T) {
	stateDir, kept, pruned := transcriptFixture(t)

	result, stderr, code := runTranscripts(t, "--apply")
	if code != exitSatisfied {
		t.Fatalf("exit = %d stderr = %q", code, stderr)
	}
	if !result.Applied || result.Copied != 1 {
		t.Fatalf("result = %+v, want one conversation copied", result)
	}

	mirror := watch.TranscriptMirrorPath(stateDir, kept)
	if !watch.TranscriptMirrorUsable(mirror) {
		t.Fatal("the rescued conversation has no usable mirror")
	}
	records, err := watch.TranscriptMirrorRecords(mirror)
	if err != nil || len(records) != 1 {
		t.Fatalf("mirror = %d records (%v), want the conversation", len(records), err)
	}
	if _, err := os.Stat(watch.TranscriptMirrorPath(stateDir, pruned)); err == nil {
		t.Fatal("a mirror was invented for a conversation that no longer exists")
	}

	// Running it again is the normal case, not a mistake.
	second, _, secondCode := runTranscripts(t, "--apply")
	if secondCode != exitSatisfied {
		t.Fatalf("second run exit = %d", secondCode)
	}
	if second.Copied != 0 || second.AlreadyKept != 1 {
		t.Fatalf("second run = %+v, want nothing copied and one already kept", second)
	}
}

func TestTranscriptsRefusesContradictoryFlags(t *testing.T) {
	transcriptFixture(t)
	_, _, code := runTranscripts(t, "--apply", "--dry-run")
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
}
