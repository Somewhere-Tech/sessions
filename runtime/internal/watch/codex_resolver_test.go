package watch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCodexFreshSessionDirsIncludeTodayYesterdayAndCreatedAt(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, time.July, 16, 23, 59, 0, 0, time.Local)
	createdAt := time.Date(2026, time.July, 2, 8, 0, 0, 0, time.Local)
	want := []string{
		filepath.Join(root, "2026", "07", "16"),
		filepath.Join(root, "2026", "07", "15"),
		filepath.Join(root, "2026", "07", "02"),
	}
	if got := CodexFreshSessionDirs(root, now, createdAt); !reflect.DeepEqual(got, want) {
		t.Fatalf("CodexFreshSessionDirs() = %v, want %v", got, want)
	}
}

func TestExtractCodexResumeID(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "subcommand", args: []string{"resume", "aaaaaaaa-1111"}, want: "aaaaaaaa-1111"},
		{name: "flag", args: []string{"--resume", "bbbbbbbb-2222"}, want: "bbbbbbbb-2222"},
		{name: "equals", args: []string{"--resume=cccccccc-3333"}, want: "cccccccc-3333"},
		{name: "reject short", args: []string{"resume", "abc"}},
		{name: "reject non hex", args: []string{"--resume=not-a-uuid"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ExtractCodexResumeID(test.args); got != test.want {
				t.Fatalf("ExtractCodexResumeID() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestResolveCodexRolloutReasons(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.Local)
	createdAt := now.Add(-time.Minute)
	targetCWD := "/tmp/sessions-target"

	t.Run("no-dir", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "missing")
		got := resolveCodexFixture(root, targetCWD, nil, createdAt, now)
		if got.Reason != CodexNoDir {
			t.Fatalf("reason = %q, want %q", got.Reason, CodexNoDir)
		}
	})

	t.Run("empty-dir", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(codexDateDir(root, now), 0o755); err != nil {
			t.Fatal(err)
		}
		got := resolveCodexFixture(root, targetCWD, nil, createdAt, now)
		if got.Reason != CodexEmptyDir {
			t.Fatalf("reason = %q, want %q", got.Reason, CodexEmptyDir)
		}
	})

	t.Run("no-cwd-match", func(t *testing.T) {
		root := t.TempDir()
		writeRolloutFixture(t, filepath.Join(codexDateDir(root, now), "rollout-other.jsonl"), "/tmp/other", now, "")
		got := resolveCodexFixture(root, targetCWD, nil, createdAt, now)
		if got.Reason != CodexNoCWDMatch {
			t.Fatalf("reason = %q, want %q", got.Reason, CodexNoCWDMatch)
		}
	})

	t.Run("no-after-spawn", func(t *testing.T) {
		root := t.TempDir()
		writeRolloutFixture(t, filepath.Join(codexDateDir(root, now), "rollout-old.jsonl"), targetCWD, createdAt.Add(-time.Second), "")
		got := resolveCodexFixture(root, targetCWD, nil, createdAt, now)
		if got.Reason != CodexNoAfterSpawn {
			t.Fatalf("reason = %q, want %q", got.Reason, CodexNoAfterSpawn)
		}
	})

	t.Run("fresh-match chooses earliest after spawn", func(t *testing.T) {
		root := t.TempDir()
		early := filepath.Join(codexDateDir(root, now), "rollout-early.jsonl")
		late := filepath.Join(codexDateDir(root, now), "rollout-late.jsonl")
		writeRolloutFixture(t, late, targetCWD, createdAt.Add(20*time.Second), "")
		writeRolloutFixture(t, early, targetCWD, createdAt.Add(10*time.Second), "")
		got := resolveCodexFixture(root, targetCWD, nil, createdAt, now)
		if got.Reason != CodexFreshMatch || got.Path != early || got.AmbiguousCount != 2 {
			t.Fatalf("resolution = %+v, want path %q reason %q ambiguity 2", got, early, CodexFreshMatch)
		}
	})
}

func TestResolveCodexRolloutMatchesExpectedInputInsteadOfSharedTimestampWindow(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, time.August, 5, 13, 30, 0, 0, time.Local)
	createdAt := now.Add(-time.Second)
	cwd := "/tmp/shared-codex-cwd"
	one := filepath.Join(codexDateDir(root, now), "rollout-one.jsonl")
	two := filepath.Join(codexDateDir(root, now), "rollout-two.jsonl")
	writeRolloutFixtureWithPrompt(t, one, cwd, now, "Reply only: ONE")
	writeRolloutFixtureWithPrompt(t, two, cwd, now.Add(time.Millisecond), "Reply only: TWO")

	got := ResolveCodexRolloutPath(CodexResolveOptions{
		CWD: cwd, CreatedAt: createdAt, SessionsDir: root, Now: now,
		ExpectedInput: "\x1b[200~Reply only: TWO\x1b[201~",
	})
	if got.Path != two || got.Reason != CodexInputMatch {
		t.Fatalf("resolution = %+v, want input-bound path %q", got, two)
	}
}

func TestResolveCodexRolloutRefusesDuplicateExpectedInput(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, time.August, 5, 13, 30, 0, 0, time.Local)
	cwd := "/tmp/shared-codex-cwd"
	writeRolloutFixtureWithPrompt(t, filepath.Join(codexDateDir(root, now), "rollout-one.jsonl"), cwd, now, "same")
	writeRolloutFixtureWithPrompt(t, filepath.Join(codexDateDir(root, now), "rollout-two.jsonl"), cwd, now.Add(time.Millisecond), "same")

	got := ResolveCodexRolloutPath(CodexResolveOptions{
		CWD: cwd, CreatedAt: now.Add(-time.Second), SessionsDir: root, Now: now, ExpectedInput: "same",
	})
	if got.Path != "" || got.Reason != CodexInputAmbiguous || got.AmbiguousCount != 2 {
		t.Fatalf("resolution = %+v, want safe ambiguous refusal", got)
	}
}

func TestResolveCodexRolloutNormalizesCWDRealpaths(t *testing.T) {
	fixtureRoot := t.TempDir()
	realCWD := filepath.Join(fixtureRoot, "private", "tmp")
	aliasCWD := filepath.Join(fixtureRoot, "tmp")
	if err := os.MkdirAll(realCWD, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realCWD, aliasCWD); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		launchCWD string
		metaCWD   string
	}{
		{name: "fixture alias to realpath", launchCWD: aliasCWD, metaCWD: realCWD},
		{name: "fixture realpath to alias", launchCWD: realCWD, metaCWD: aliasCWD},
	}
	if normalizeCWD("/tmp") == "/private/tmp" {
		tests = append(tests, struct {
			name      string
			launchCWD string
			metaCWD   string
		}{name: "macOS /tmp to /private/tmp", launchCWD: "/tmp", metaCWD: "/private/tmp"})
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.Local)
			createdAt := now.Add(-time.Second)
			path := filepath.Join(codexDateDir(root, now), "rollout-realpath.jsonl")
			writeRolloutFixture(t, path, test.metaCWD, now, "")
			got := resolveCodexFixture(root, test.launchCWD, nil, createdAt, now)
			if got.Reason != CodexFreshMatch || got.Path != path {
				t.Fatalf("resolution = %+v, want normalized cwd match %q", got, path)
			}
		})
	}
}

func TestResolveCodexRolloutReadsBeyond16KiB(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.Local)
	createdAt := now.Add(-time.Second)
	path := filepath.Join(codexDateDir(root, now), "rollout-large-meta.jsonl")
	writeRolloutFixture(t, path, "/tmp/large-meta-target", now, strings.Repeat("x", 20*1024))

	line, err := readCodexFirstLine(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(line) <= 16*1024 || len(line) >= codexFirstLineBytes {
		t.Fatalf("fixture line length = %d, want >16KiB and <64KiB", len(line))
	}
	got := resolveCodexFixture(root, "/tmp/large-meta-target", nil, createdAt, now)
	if got.Reason != CodexFreshMatch || got.Path != path {
		t.Fatalf("resolution = %+v, want 64KiB read to resolve %q", got, path)
	}
	t.Logf("resolved session_meta first line of %d bytes (>16KiB) with reason %s", len(line), got.Reason)
}

func TestResolveCodexRolloutFullScanNearestTimestamp(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.Local)
	createdAt := now.Add(-7 * 24 * time.Hour)
	targetCWD := "/tmp/fullscan-target"
	closest := filepath.Join(root, "2000", "01", "01", "rollout-closest.jsonl")
	later := filepath.Join(root, "2000", "01", "02", "rollout-much-later.jsonl")
	boundedOther := filepath.Join(codexDateDir(root, createdAt), "rollout-other-cwd.jsonl")
	writeRolloutFixture(t, closest, targetCWD, createdAt.Add(-time.Minute), "")
	writeRolloutFixture(t, later, targetCWD, createdAt.Add(time.Hour), "")
	writeRolloutFixture(t, boundedOther, "/tmp/other", createdAt.Add(time.Second), "")

	got := resolveCodexFixture(root, targetCWD, nil, createdAt, now)
	if got.Reason != CodexFreshFullScan || got.Path != closest || got.AmbiguousCount != 2 {
		t.Fatalf("resolution = %+v, want path %q reason %q ambiguity 2", got, closest, CodexFreshFullScan)
	}
	t.Logf("out-of-window rollout resolved with reason %s among %d cwd matches", got.Reason, got.AmbiguousCount)
}

func TestResolveCodexRolloutResumeIsGlobalAndNewest(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.Local)
	resumeID := "aaaaaaaa-1111-2222-3333-444444444444"
	older := filepath.Join(root, "2000", "01", "01", "rollout-old-"+resumeID+".jsonl")
	newer := filepath.Join(root, "2001", "01", "01", "rollout-new-"+resumeID+".jsonl")
	writeRolloutFixture(t, older, "/tmp/old", now.Add(-time.Hour), "")
	writeRolloutFixture(t, newer, "/tmp/new", now.Add(-time.Hour), "")
	if err := os.Chtimes(older, now.Add(-time.Minute), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, now, now); err != nil {
		t.Fatal(err)
	}

	got := resolveCodexFixture(root, "/cwd-is-ignored-for-resume", []string{"resume", resumeID}, now, now)
	if got.Reason != CodexResumeMatch || got.Path != newer {
		t.Fatalf("resolution = %+v, want global newest resume %q", got, newer)
	}
	missing := resolveCodexFixture(root, "", []string{"--resume=bbbbbbbb-2222"}, now, now)
	if missing.Reason != CodexResumeMissing || missing.Path != "" {
		t.Fatalf("missing resolution = %+v", missing)
	}
}

func resolveCodexFixture(root, cwd string, args []string, createdAt, now time.Time) CodexResolution {
	return ResolveCodexRolloutPath(CodexResolveOptions{
		CWD:         cwd,
		Args:        args,
		CreatedAt:   createdAt,
		SessionsDir: root,
		Now:         now,
	})
}

func writeRolloutFixture(t *testing.T, path, cwd string, timestamp time.Time, padding string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	record := map[string]any{
		"timestamp": timestamp.Format(time.RFC3339Nano),
		"type":      "session_meta",
		"payload": map[string]any{
			"context":   padding,
			"cwd":       cwd,
			"timestamp": timestamp.Format(time.RFC3339Nano),
		},
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeRolloutFixtureWithPrompt(t *testing.T, path, cwd string, timestamp time.Time, prompt string) {
	t.Helper()
	writeRolloutFixture(t, path, cwd, timestamp, "")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := json.NewEncoder(file).Encode(map[string]any{
		"timestamp": timestamp.Format(time.RFC3339Nano),
		"type":      "response_item",
		"payload": map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": prompt}},
		},
	}); err != nil {
		t.Fatal(err)
	}
}

// History resolution has one shot at naming the right rollout. With
// StrictStart set, a session that recorded no thread id and sent no first
// message claims a rollout only when exactly one started with it; the
// nearest-timestamp full scan that suits a live watcher is not attempted.
func TestResolveCodexRolloutStrictStartRefusesToGuess(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, time.September, 2, 9, 0, 0, 0, time.Local)
	createdAt := now.Add(-time.Hour)
	cwd := "/tmp/strict-target"
	dir := codexDateDir(root, createdAt)

	// One rollout six minutes after the session started: someone else's.
	late := filepath.Join(dir, "rollout-late-0000000a.jsonl")
	writeRolloutFixture(t, late, cwd, createdAt.Add(6*time.Minute), "")
	got := ResolveCodexRolloutPath(CodexResolveOptions{CWD: cwd, CreatedAt: createdAt, SessionsDir: root, Now: now, StrictStart: 5 * time.Minute})
	if got.Reason != CodexUnclaimed || got.Path != "" {
		t.Fatalf("late rollout resolution = %+v, want unclaimed", got)
	}

	// The same lookup without StrictStart still guesses, as a live watcher may.
	if loose := ResolveCodexRolloutPath(CodexResolveOptions{CWD: cwd, CreatedAt: createdAt, SessionsDir: root, Now: now}); loose.Path != late {
		t.Fatalf("loose resolution = %+v, want %q", loose, late)
	}

	// A single rollout that started seconds after the session is claimed.
	early := filepath.Join(dir, "rollout-early-0000000b.jsonl")
	writeRolloutFixture(t, early, cwd, createdAt.Add(3*time.Second), "")
	got = ResolveCodexRolloutPath(CodexResolveOptions{CWD: cwd, CreatedAt: createdAt, SessionsDir: root, Now: now, StrictStart: 5 * time.Minute})
	if got.Reason != CodexUnclaimed || got.AmbiguousCount != 2 {
		t.Fatalf("two candidates = %+v, want unclaimed with ambiguity 2", got)
	}
	if err := os.Remove(late); err != nil {
		t.Fatal(err)
	}
	got = ResolveCodexRolloutPath(CodexResolveOptions{CWD: cwd, CreatedAt: createdAt, SessionsDir: root, Now: now, StrictStart: 5 * time.Minute})
	if got.Reason != CodexFreshMatch || got.Path != early {
		t.Fatalf("single close rollout = %+v, want %q", got, early)
	}

	// A recorded thread id identifies the rollout wherever it is.
	elsewhere := filepath.Join(root, "2000", "01", "01", "rollout-2000-01-01T00-00-00-0000000c-aaaa-bbbb-cccc-dddddddddddd.jsonl")
	writeRolloutFixture(t, elsewhere, "/tmp/other", createdAt.Add(-time.Hour), "")
	got = ResolveCodexRolloutPath(CodexResolveOptions{CWD: cwd, CreatedAt: createdAt, SessionsDir: root, Now: now, StrictStart: 5 * time.Minute, ConversationID: "0000000c-aaaa-bbbb-cccc-dddddddddddd"})
	if got.Reason != CodexResumeMatch || got.Path != elsewhere {
		t.Fatalf("thread id resolution = %+v, want %q", got, elsewhere)
	}
}
