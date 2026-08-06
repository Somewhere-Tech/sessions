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
func adoptFixture(t *testing.T) (*adoptTestCreator, recovery.Adoption, func(recovery.AdoptOptions) (recovery.AdoptResult, error)) {
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
	return creator, adoption, run
}

// Resuming a conversation the user has open in their own terminal gives one
// conversation two writers, and whichever loses is what the provider
// transcript ends up without.
func TestAdoptRefusesAConversationOpenInSomebodyElsesProcess(t *testing.T) {
	registry := t.TempDir()
	writeLiveRegistryEntry(t, registry, 4321, openConversationUUID, "pretty-pty-02")
	creator, _, run := adoptFixture(t)

	_, err := run(recovery.AdoptOptions{ClaudeLive: &watch.ClaudeLiveQuery{
		Dir:   registry,
		Alive: func(int) bool { return true },
	}})
	if err == nil {
		t.Fatal("adoption proceeded into a conversation that was already open elsewhere")
	}
	if creator.calls != 0 {
		t.Fatalf("a session was created anyway (%d calls)", creator.calls)
	}
	// The refusal has to say what to do about it, not only what happened.
	for _, want := range []string{"already open", "pretty-pty-02", "4321", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err.Error(), want)
		}
	}
}

// --force is the user overriding a warning they have now read.
func TestForceOverridesTheDoubleOpenRefusal(t *testing.T) {
	registry := t.TempDir()
	writeLiveRegistryEntry(t, registry, 4321, openConversationUUID, "pretty-pty-02")
	creator, _, run := adoptFixture(t)

	if _, err := run(recovery.AdoptOptions{Force: true, ClaudeLive: &watch.ClaudeLiveQuery{
		Dir:   registry,
		Alive: func(int) bool { return true },
	}}); err != nil {
		t.Fatalf("--force did not override the refusal: %v", err)
	}
	if creator.calls != 1 {
		t.Fatalf("creator calls = %d, want 1", creator.calls)
	}
}

// Claude does not clean up entries for processes that died, so refusing on a
// dead claim would make adoption fail forever after one crash.
func TestAdoptProceedsWhenTheOnlyClaimIsADeadProcess(t *testing.T) {
	registry := t.TempDir()
	writeLiveRegistryEntry(t, registry, 4321, openConversationUUID, "crashed-session")
	creator, _, run := adoptFixture(t)

	if _, err := run(recovery.AdoptOptions{ClaudeLive: &watch.ClaudeLiveQuery{
		Dir:   registry,
		Alive: func(int) bool { return false },
	}}); err != nil {
		t.Fatalf("a stale registry entry blocked adoption: %v", err)
	}
	if creator.calls != 1 {
		t.Fatalf("creator calls = %d, want 1", creator.calls)
	}
}

// "We could not check" must never be recorded as "we checked and it was
// free". An older Claude with no registry has to stay visible to the caller.
func TestAdoptRecordsAnInconclusiveLiveCheck(t *testing.T) {
	_, _, run := adoptFixture(t)

	result, err := run(recovery.AdoptOptions{ClaudeLive: &watch.ClaudeLiveQuery{
		Dir: filepath.Join(t.TempDir(), "no-such-registry"),
	}})
	if err != nil {
		t.Fatalf("a missing registry blocked adoption: %v", err)
	}
	if !result.LiveCheckInconclusive {
		t.Fatal("an unreadable registry was reported as a clean check")
	}
}

// A conversation held by a process Sessions launched is not somebody else's.
// Sessions runs claude as a child, so ownership is decided by ancestry.
func TestAdoptDoesNotRefuseAConversationSessionsOwns(t *testing.T) {
	registry := t.TempDir()
	writeLiveRegistryEntry(t, registry, 4321, openConversationUUID, "sessions-owned")
	creator, _, run := adoptFixture(t)

	if _, err := run(recovery.AdoptOptions{ClaudeLive: &watch.ClaudeLiveQuery{
		Dir:       registry,
		Alive:     func(int) bool { return true },
		Parents:   func() map[int]int { return map[int]int{4321: 999} },
		OwnedPIDs: []int{999},
	}}); err != nil {
		t.Fatalf("Sessions refused a conversation it owns itself: %v", err)
	}
	if creator.calls != 1 {
		t.Fatalf("creator calls = %d, want 1", creator.calls)
	}
}
