package recovery_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

const openConversationUUID = "3fe0b590-6916-41cc-a9b8-3b6c4e75fa17"

// writeLiveRegistryEntry writes one <pid>.json in the shape Claude Code
// actually uses, taken from real files in ~/.claude/sessions.
func writeLiveRegistryEntry(t *testing.T, dir string, pid int, conversation, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(map[string]any{
		"pid": pid, "sessionId": conversation, "cwd": "/Users/someone/work",
		"name": name, "status": "waiting", "waitingFor": "permission prompt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", pid)), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

// adoptFixture builds the ledger, creator, and adoption used by every case
// here, so each test differs only in the registry it is pointed at.
func adoptFixture(t *testing.T) (*adoptTestCreator, func(recovery.AdoptOptions) (recovery.AdoptResult, error)) {
	t.Helper()
	root := t.TempDir()
	store := openScratchLedger(t, root)
	creator := &adoptTestCreator{
		boundaries:   store.Boundaries(),
		laneID:       "20000000-0000-4000-8000-0000000000f1",
		providerUUID: openConversationUUID,
	}
	adoption := recovery.Adoption{
		Path: filepath.Join(root, "conversation.jsonl"),
		Tool: string(state.ToolClaude), Cwd: root, ProviderUUID: openConversationUUID,
		Cmd: "claude", Args: []string{"--resume", openConversationUUID},
	}
	run := func(options recovery.AdoptOptions) (recovery.AdoptResult, error) {
		options.Events = store
		return recovery.Adopt(context.Background(), adoption, "resumed", creator,
			store.Boundaries(), store.Observations(), options)
	}
	return creator, run
}

// Sessions cannot make a provider conversation exclusive, so it does not
// pretend to: resuming one that is open elsewhere proceeds, and simply says
// where else it is open. The mirror is what makes that safe -- it is
// append-only, so two writers colliding on the provider transcript still
// leaves Sessions holding the union.
func TestAdoptReportsAnotherOpenProcessWithoutRefusing(t *testing.T) {
	registry := t.TempDir()
	writeLiveRegistryEntry(t, registry, 4321, openConversationUUID, "pretty-pty-02")
	creator, run := adoptFixture(t)

	result, err := run(recovery.AdoptOptions{ClaudeLive: &watch.ClaudeLiveQuery{
		Dir:   registry,
		Alive: func(int) bool { return true },
	}})
	if err != nil {
		t.Fatalf("adoption refused instead of reporting: %v", err)
	}
	if creator.calls != 1 {
		t.Fatalf("creator calls = %d, want the session created", creator.calls)
	}
	for _, want := range []string{"pretty-pty-02", "4321"} {
		if !strings.Contains(result.AlsoOpenIn, want) {
			t.Fatalf("AlsoOpenIn = %q, want it to name %q", result.AlsoOpenIn, want)
		}
	}
}

// A dead entry claiming the conversation is not a holder, and is not worth
// telling anyone about. Claude never cleans these up.
func TestAdoptSaysNothingAboutADeadProcess(t *testing.T) {
	registry := t.TempDir()
	writeLiveRegistryEntry(t, registry, 4321, openConversationUUID, "crashed-session")
	creator, run := adoptFixture(t)

	result, err := run(recovery.AdoptOptions{ClaudeLive: &watch.ClaudeLiveQuery{
		Dir:   registry,
		Alive: func(int) bool { return false },
	}})
	if err != nil {
		t.Fatalf("a stale registry entry disturbed adoption: %v", err)
	}
	if creator.calls != 1 || result.AlsoOpenIn != "" {
		t.Fatalf("calls=%d AlsoOpenIn=%q, want a clean adoption with nothing reported",
			creator.calls, result.AlsoOpenIn)
	}
}

// A conversation held by a process Sessions launched is not somebody else's,
// so there is nothing to tell the user about it.
func TestAdoptSaysNothingAboutAProcessSessionsOwns(t *testing.T) {
	registry := t.TempDir()
	writeLiveRegistryEntry(t, registry, 4321, openConversationUUID, "sessions-owned")
	creator, run := adoptFixture(t)

	result, err := run(recovery.AdoptOptions{ClaudeLive: &watch.ClaudeLiveQuery{
		Dir:       registry,
		Alive:     func(int) bool { return true },
		Parents:   func() map[int]int { return map[int]int{4321: 999} },
		OwnedPIDs: []int{999},
	}})
	if err != nil {
		t.Fatalf("adoption of a conversation Sessions owns failed: %v", err)
	}
	if creator.calls != 1 || result.AlsoOpenIn != "" {
		t.Fatalf("calls=%d AlsoOpenIn=%q, want a clean adoption with nothing reported",
			creator.calls, result.AlsoOpenIn)
	}
}

// An unreadable or absent registry changes nothing. There is no "we could not
// check" state to reason about, because nothing was ever gated on the answer.
func TestAdoptIsUnaffectedByAnAbsentRegistry(t *testing.T) {
	creator, run := adoptFixture(t)

	result, err := run(recovery.AdoptOptions{ClaudeLive: &watch.ClaudeLiveQuery{
		Dir: filepath.Join(t.TempDir(), "no-such-registry"),
	}})
	if err != nil {
		t.Fatalf("a missing registry disturbed adoption: %v", err)
	}
	if creator.calls != 1 || result.AlsoOpenIn != "" {
		t.Fatalf("calls=%d AlsoOpenIn=%q, want a clean adoption with nothing reported",
			creator.calls, result.AlsoOpenIn)
	}
}
