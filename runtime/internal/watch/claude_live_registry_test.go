package watch

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// writeLiveEntry writes one registry file in the exact shape observed in a real
// ~/.claude/sessions directory. Keeping the fixture keyed by pid matters: the
// filename is the pid by construction and the parser relies on that.
func writeLiveEntry(t *testing.T, dir string, entry map[string]any) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pid, ok := entry["pid"].(int)
	if !ok {
		t.Fatalf("fixture entry needs an int pid, got %v", entry["pid"])
	}
	path := filepath.Join(dir, strconv.Itoa(pid)+".json")
	payload, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// liveEntry is a verbatim copy of an entry read from the real registry on the
// development machine, with the pid parameterised. If Claude changes this shape
// the parser has to be revisited, so the fixture is the observed one rather
// than a minimal invention.
func liveEntry(pid int, sessionID, cwd string) map[string]any {
	return map[string]any{
		"pid":             pid,
		"sessionId":       sessionID,
		"cwd":             cwd,
		"startedAt":       1785878763246,
		"procStart":       "Tue Aug  4 21:26:02 2026",
		"version":         "2.1.220",
		"peerProtocol":    1,
		"kind":            "interactive",
		"entrypoint":      "cli",
		"name":            "pretty-pty-02",
		"nameSource":      "derived",
		"status":          "waiting",
		"updatedAt":       1785970422065,
		"statusUpdatedAt": 1785970422065,
		"waitingFor":      "permission prompt",
	}
}

func TestClaudeLiveRegistryParsesObservedShape(t *testing.T) {
	dir := t.TempDir()
	path := writeLiveEntry(t, dir, liveEntry(22440, "3fe0b590-6916-41cc-a9b8-3b6c4e75fa17", "/Users/uzair/pretty-PTY"))

	sessions, err := ReadClaudeLiveRegistry(ClaudeLiveQuery{
		Dir:   dir,
		Alive: func(int) bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("read %d entries, want 1", len(sessions))
	}
	got := sessions[0]
	if got.PID != 22440 {
		t.Fatalf("pid = %d, want 22440", got.PID)
	}
	if got.ProviderSessionID != "3fe0b590-6916-41cc-a9b8-3b6c4e75fa17" {
		t.Fatalf("sessionId = %q", got.ProviderSessionID)
	}
	if got.CWD != "/Users/uzair/pretty-PTY" {
		t.Fatalf("cwd = %q", got.CWD)
	}
	if got.Status != ClaudeStatusWaiting || got.WaitingFor != "permission prompt" {
		t.Fatalf("status = %q/%q, want waiting/permission prompt", got.Status, got.WaitingFor)
	}
	if got.Kind != "interactive" || got.Entrypoint != "cli" || got.Version != "2.1.220" {
		t.Fatalf("provenance fields = %+v", got)
	}
	if got.RegistryPath != path {
		t.Fatalf("registryPath = %q, want %q", got.RegistryPath, path)
	}
	if got.Busy() {
		t.Fatal("a waiting entry must not report Busy")
	}
}

// TestClaudeConversationOpenDetectsExternalHolder is the case the whole file
// exists for: the user has the conversation open in their own terminal, and
// Sessions is about to adopt or resume it.
func TestClaudeConversationOpenDetectsExternalHolder(t *testing.T) {
	dir := t.TempDir()
	const conversation = "3fe0b590-6916-41cc-a9b8-3b6c4e75fa17"
	writeLiveEntry(t, dir, liveEntry(22440, conversation, "/Users/uzair/pretty-PTY"))

	check := ClaudeConversationOpen(conversation, ClaudeLiveQuery{
		Dir:   dir,
		Alive: func(pid int) bool { return pid == 22440 },
	})
	if check.Reason != ClaudeLiveExternal {
		t.Fatalf("reason = %q, want %q", check.Reason, ClaudeLiveExternal)
	}
	if !check.Open || !check.External {
		t.Fatalf("open=%v external=%v, want both true", check.Open, check.External)
	}
	if check.Holder == nil || check.Holder.PID != 22440 {
		t.Fatalf("holder = %+v, want pid 22440", check.Holder)
	}
}

// TestClaudeConversationOpenIgnoresDeadProcess keeps a crashed Claude from
// locking the user out of their own conversation forever. The leftover file is
// reported, never deleted.
func TestClaudeConversationOpenIgnoresDeadProcess(t *testing.T) {
	dir := t.TempDir()
	const conversation = "dead-conversation"
	path := writeLiveEntry(t, dir, liveEntry(4242, conversation, "/tmp/work"))

	check := ClaudeConversationOpen(conversation, ClaudeLiveQuery{
		Dir:   dir,
		Alive: func(int) bool { return false },
	})
	if check.Reason != ClaudeLiveStale {
		t.Fatalf("reason = %q, want %q", check.Reason, ClaudeLiveStale)
	}
	if check.Open || check.External {
		t.Fatalf("a dead pid must not read as open: %+v", check)
	}
	if len(check.Stale) != 1 || check.Stale[0].PID != 4242 {
		t.Fatalf("stale = %+v, want the leftover entry", check.Stale)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the registry belongs to Claude and must not be modified: %v", err)
	}
}

// TestClaudeConversationOpenTreatsOwnedProcessSeparately distinguishes "someone
// else has it" from "we already have it". Both are open; only the first is
// destructive.
func TestClaudeConversationOpenTreatsOwnedProcessSeparately(t *testing.T) {
	dir := t.TempDir()
	const conversation = "owned-conversation"
	writeLiveEntry(t, dir, liveEntry(910, conversation, "/tmp/work"))

	check := ClaudeConversationOpen(conversation, ClaudeLiveQuery{
		Dir:       dir,
		OwnedPIDs: []int{910},
		Alive:     func(int) bool { return true },
	})
	if check.Reason != ClaudeLiveOwned {
		t.Fatalf("reason = %q, want %q", check.Reason, ClaudeLiveOwned)
	}
	if !check.Open || check.External {
		t.Fatalf("owned holder should be open but not external: %+v", check)
	}
}

// TestClaudeConversationOpenResolvesOwnershipByAncestry is the realistic
// arrangement: Sessions runs `claude` as a child of its runner, so the registry
// pid is never the runner pid and ownership can only come from the process
// tree.
func TestClaudeConversationOpenResolvesOwnershipByAncestry(t *testing.T) {
	dir := t.TempDir()
	const conversation = "descendant-conversation"
	writeLiveEntry(t, dir, liveEntry(3003, conversation, "/tmp/work"))

	// 3003 (claude) -> 2002 (shell) -> 1001 (sessions runner)
	parents := map[int]int{3003: 2002, 2002: 1001, 1001: 1}

	owned := ClaudeConversationOpen(conversation, ClaudeLiveQuery{
		Dir:       dir,
		OwnedPIDs: []int{1001},
		Alive:     func(int) bool { return true },
		Parents:   func() map[int]int { return parents },
	})
	if owned.Reason != ClaudeLiveOwned {
		t.Fatalf("reason = %q, want %q for a descendant of a runner", owned.Reason, ClaudeLiveOwned)
	}

	// The same tree with an unrelated runner stays external.
	external := ClaudeConversationOpen(conversation, ClaudeLiveQuery{
		Dir:       dir,
		OwnedPIDs: []int{7777},
		Alive:     func(int) bool { return true },
		Parents:   func() map[int]int { return parents },
	})
	if external.Reason != ClaudeLiveExternal {
		t.Fatalf("reason = %q, want %q", external.Reason, ClaudeLiveExternal)
	}
}

// TestClaudeOwnershipSurvivesCyclicProcessTable guards the ancestry walk against
// a corrupt or racing snapshot rather than spinning inside a poll loop.
func TestClaudeOwnershipSurvivesCyclicProcessTable(t *testing.T) {
	ownership := newClaudeOwnership([]int{99}, func() map[int]int {
		return map[int]int{1: 2, 2: 3, 3: 1}
	})
	if ownership.owns(1) {
		t.Fatal("a cycle that never reaches an owned pid must not report ownership")
	}
}

// TestClaudeConversationOpenReportsUnavailableRegistry is the safety-critical
// distinction. A missing registry is "cannot tell", and a caller that treats it
// as "not open" reintroduces exactly the silent overwrite this check prevents.
func TestClaudeConversationOpenReportsUnavailableRegistry(t *testing.T) {
	check := ClaudeConversationOpen("anything", ClaudeLiveQuery{
		Dir:   filepath.Join(t.TempDir(), "no-such-registry"),
		Alive: func(int) bool { return true },
	})
	if check.Reason != ClaudeLiveUnknown {
		t.Fatalf("reason = %q, want %q", check.Reason, ClaudeLiveUnknown)
	}
	if check.Open || check.External {
		t.Fatalf("unknown must not claim openness: %+v", check)
	}
	if !errors.Is(check.Err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want a not-exist error the caller can distinguish", check.Err)
	}
}

// TestClaudeConversationOpenWithNoProviderIDIsUnknown covers a session whose
// provider identity was never recorded. There is nothing to match on, and
// answering "not open" would be an unfounded all-clear.
func TestClaudeConversationOpenWithNoProviderIDIsUnknown(t *testing.T) {
	dir := t.TempDir()
	writeLiveEntry(t, dir, liveEntry(1, "some-conversation", "/tmp/work"))
	check := ClaudeConversationOpen("  ", ClaudeLiveQuery{Dir: dir, Alive: func(int) bool { return true }})
	if check.Reason != ClaudeLiveUnknown || check.Open {
		t.Fatalf("check = %+v, want unknown and not open", check)
	}
}

func TestClaudeConversationOpenNotOpen(t *testing.T) {
	dir := t.TempDir()
	writeLiveEntry(t, dir, liveEntry(500, "another-conversation", "/tmp/other"))
	check := ClaudeConversationOpen("the-one-we-want", ClaudeLiveQuery{
		Dir:   dir,
		Alive: func(int) bool { return true },
	})
	if check.Reason != ClaudeLiveNotOpen || check.Open {
		t.Fatalf("check = %+v, want an affirmative all-clear", check)
	}
}

// TestClaudeLiveRegistrySkipsUnusableEntries keeps one malformed or half-written
// file from blinding the scan to every other live process.
func TestClaudeLiveRegistrySkipsUnusableEntries(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Mid-write truncation.
	if err := os.WriteFile(filepath.Join(dir, "111.json"), []byte(`{"pid":111,"sessi`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Valid JSON with no conversation id: cannot answer the question.
	if err := os.WriteFile(filepath.Join(dir, "222.json"), []byte(`{"pid":222}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Not a registry file at all.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignore me"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeLiveEntry(t, dir, liveEntry(333, "good", "/tmp/work"))

	sessions, err := ReadClaudeLiveRegistry(ClaudeLiveQuery{Dir: dir, Alive: func(int) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].PID != 333 {
		t.Fatalf("sessions = %+v, want only the usable entry", sessions)
	}
}

// TestClaudeLiveRegistryFallsBackToFilenamePID keeps an entry usable when its
// body lost the pid. The filename is the pid by construction.
func TestClaudeLiveRegistryFallsBackToFilenamePID(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "8080.json"),
		[]byte(`{"sessionId":"conv","cwd":"/tmp/work"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sessions, err := ReadClaudeLiveRegistry(ClaudeLiveQuery{Dir: dir, Alive: func(int) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].PID != 8080 {
		t.Fatalf("sessions = %+v, want pid recovered from the filename", sessions)
	}
}

// TestClaudeConversationOpenPrefersExternalHolder covers a conversation claimed
// by both an owned and an external live process. Reporting the owned one would
// understate the risk, and the external one is the reason to refuse.
func TestClaudeConversationOpenPrefersExternalHolder(t *testing.T) {
	dir := t.TempDir()
	const conversation = "contested"
	writeLiveEntry(t, dir, liveEntry(100, conversation, "/tmp/work"))
	writeLiveEntry(t, dir, liveEntry(200, conversation, "/tmp/work"))

	check := ClaudeConversationOpen(conversation, ClaudeLiveQuery{
		Dir:       dir,
		OwnedPIDs: []int{100},
		Alive:     func(int) bool { return true },
	})
	if check.Reason != ClaudeLiveExternal || check.Holder == nil || check.Holder.PID != 200 {
		t.Fatalf("check = %+v, want the external holder to win", check)
	}
}

func TestClaudeConversationsOpenByCWD(t *testing.T) {
	dir := t.TempDir()
	work := t.TempDir()
	writeLiveEntry(t, dir, liveEntry(11, "conv-a", work))
	writeLiveEntry(t, dir, liveEntry(12, "conv-b", filepath.Join(work, "elsewhere")))
	writeLiveEntry(t, dir, liveEntry(13, "conv-c", work))

	matches := ClaudeConversationsOpenByCWD(work, ClaudeLiveQuery{
		Dir:   dir,
		Alive: func(pid int) bool { return pid != 13 },
	})
	if len(matches) != 1 || matches[0].PID != 11 {
		t.Fatalf("matches = %+v, want only the live entry in that cwd", matches)
	}
}

// TestProcessAliveAgreesWithItself pins the probe against processes whose state
// is known for certain: this test binary is alive, and pid 0 never names one.
func TestProcessAliveAgreesWithItself(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("processAlive reported this running test as dead")
	}
	if processAlive(0) || processAlive(-1) {
		t.Fatal("processAlive accepted a non-pid")
	}
}

// TestProcessParentsSeesThisProcess verifies the real snapshot rather than only
// the injected one, since ancestry is what decides owned versus external.
func TestProcessParentsSeesThisProcess(t *testing.T) {
	parents := processParents()
	if len(parents) == 0 {
		t.Skip("no process table available in this environment")
	}
	got, ok := parents[os.Getpid()]
	if !ok {
		t.Fatalf("process table has %d entries but not this pid %d", len(parents), os.Getpid())
	}
	if got != os.Getppid() {
		t.Fatalf("parent of %d = %d, want %d", os.Getpid(), got, os.Getppid())
	}
}
