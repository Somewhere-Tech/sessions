package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// newPrincipalManager builds a manager with the attribution machinery wired up,
// because telling a human from an agent is exactly what attribution decides.
func newPrincipalManager(t *testing.T) (*Manager, *prototest.Launcher, string) {
	t.Helper()
	root := t.TempDir()
	store, err := ledger.Open(context.Background(), ledger.Options{
		Path: filepath.Join(root, "ledger", "lanes.sqlite3"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	launcher := prototest.NewLauncher()
	manager := NewManager(testConfig(root), launcher, ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(),
		Attributions: store.Attributions(), LedgerReader: store,
	})
	t.Cleanup(manager.Close)
	return manager, launcher, root
}

func principalInfo(t *testing.T, manager *Manager, id string) state.SessionInfo {
	t.Helper()
	session, ok := manager.Registry().Get(id)
	if !ok {
		t.Fatalf("session %s is not registered", id)
	}
	return session.Info()
}

// userTranscriptRecord is the shape a provider writes into its own conversation
// file for a user turn. Sessions never sees it through an input route; the
// transcript watcher hands it over after the fact.
func userTranscriptRecord(at time.Time, text string) json.RawMessage {
	encoded, err := json.Marshal(map[string]any{
		"type":      "user",
		"timestamp": at.UTC().Format(time.RFC3339Nano),
		"message":   map[string]any{"role": "user", "content": text},
	})
	if err != nil {
		panic(err)
	}
	return encoded
}

func recordProviderTranscript(t *testing.T, manager *Manager, id string, event json.RawMessage) {
	t.Helper()
	session, ok := manager.Registry().Get(id)
	if !ok {
		t.Fatalf("session %s is not registered", id)
	}
	session.RecordClaudeEvent(event)
}

func TestUnattributedInputStampsAHumanAndNotAnAgent(t *testing.T) {
	manager, launcher, root := newPrincipalManager(t)
	target, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: root, Name: "Orchestrator",
	})
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().UnixMilli()
	if !manager.Input(context.Background(), target.ID, "are you there?") {
		t.Fatal("Input did not reach the runner")
	}
	after := time.Now().UnixMilli()
	if got := launcher.Runner(target.ID).Inputs(); len(got) != 1 {
		t.Fatalf("runner inputs = %#v", got)
	}

	info := principalInfo(t, manager, target.ID)
	if info.LastHumanMessageAt == nil {
		t.Fatalf("input that arrived through Sessions with no source-session attribution " +
			"left LastHumanMessageAt unset; unattributed input is the only thing a person can send")
	}
	if *info.LastHumanMessageAt < before || *info.LastHumanMessageAt > after {
		t.Errorf("LastHumanMessageAt = %d, want within [%d,%d]", *info.LastHumanMessageAt, before, after)
	}
	if info.LastAgentMessageAt != nil {
		t.Errorf("LastAgentMessageAt = %d after a human message, want unset", *info.LastAgentMessageAt)
	}
}

func TestAttributedInputStampsAnAgentAndNotAHuman(t *testing.T) {
	manager, _, root := newPrincipalManager(t)
	source, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: root, Name: "PM Claude",
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: root, Name: "Reviewer",
	})
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().UnixMilli()
	if err := manager.InputAttributed(context.Background(), target.ID, "please review #25", state.InputAttribution{
		SourceSessionID: source.ID, Client: "sessions-cli",
	}); err != nil {
		t.Fatal(err)
	}
	after := time.Now().UnixMilli()

	info := principalInfo(t, manager, target.ID)
	if info.LastAgentMessageAt == nil {
		t.Fatalf("input carrying a source session left LastAgentMessageAt unset; " +
			"a session relaying to another session is the definition of agent contact")
	}
	if *info.LastAgentMessageAt < before || *info.LastAgentMessageAt > after {
		t.Errorf("LastAgentMessageAt = %d, want within [%d,%d]", *info.LastAgentMessageAt, before, after)
	}
	if info.LastHumanMessageAt != nil {
		t.Fatalf("one session relaying a message to another stamped LastHumanMessageAt=%d; "+
			"reporting agent traffic as human contact is how a fleet census counts sessions "+
			"nobody has touched as sessions the owner touched this week",
			*info.LastHumanMessageAt)
	}
	// The Enter that submits an attributed message is sent separately and
	// without attribution, so it must not be mistaken for a person typing.
	if !manager.Input(context.Background(), target.ID, "\r") {
		t.Fatal("submit Enter did not reach the runner")
	}
	if info := principalInfo(t, manager, target.ID); info.LastHumanMessageAt != nil {
		t.Fatalf("the Enter that submits an agent's message stamped LastHumanMessageAt=%d; "+
			"an attributed submit sends its text with attribution and its Enter without, so a "+
			"whitespace-only payload must stamp nothing", *info.LastHumanMessageAt)
	}
}

// TestAProviderInternalUserRecordStampsNeitherPrincipal is the whole point of
// these two fields. The incident that produced them was a lane's own
// self-scheduled cron tick arriving as type:"user" in the provider transcript,
// three seconds after the owner had asked a question and been buried by it.
// Nothing in the transcript distinguishes that tick from the owner. The input
// boundary does, because the tick never crosses it.
func TestAProviderInternalUserRecordStampsNeitherPrincipal(t *testing.T) {
	manager, _, root := newPrincipalManager(t)
	target, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: root, Name: "Integrator",
	})
	if err != nil {
		t.Fatal(err)
	}
	tickAt := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	recordProviderTranscript(t, manager, target.ID,
		userTranscriptRecord(tickAt, "INTEGRATOR TICK (founder-ordered 30-min cadence)"))

	info := principalInfo(t, manager, target.ID)
	if info.LastUserMessageAt == nil || *info.LastUserMessageAt != tickAt.UnixMilli() {
		t.Fatalf("LastUserMessageAt = %v, want %d: the transcript-derived field must keep "+
			"reporting every user-role record exactly as it always has",
			info.LastUserMessageAt, tickAt.UnixMilli())
	}
	if info.LastHumanMessageAt != nil {
		t.Errorf("a provider-internal user record stamped LastHumanMessageAt=%d. "+
			"This is the bug these fields exist to fix: a cron tick the provider wrote "+
			"straight into its own transcript is not a person, it never passed through "+
			"Sessions' input path, and counting it as human contact is what let a fleet "+
			"census report machine chatter as the owner's touch.",
			*info.LastHumanMessageAt)
	}
	if info.LastAgentMessageAt != nil {
		t.Errorf("a provider-internal user record stamped LastAgentMessageAt=%d. "+
			"A provider injecting into its own conversation is not another Sessions lane "+
			"relaying a message either; transcript-only user records are neither principal.",
			*info.LastAgentMessageAt)
	}
}

// TestTheCronTickDoesNotOverwriteTheOwnersLastContact reproduces the incident
// end to end: the owner speaks, the provider records his turn, and three
// seconds later the lane's own scheduled tick lands as another user record.
// The answer to "when did the user last touch this" must still be his message.
func TestTheCronTickDoesNotOverwriteTheOwnersLastContact(t *testing.T) {
	manager, _, root := newPrincipalManager(t)
	target, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: root, Name: "Main orchestrator lane",
	})
	if err != nil {
		t.Fatal(err)
	}

	if !manager.Input(context.Background(), target.ID, "?") {
		t.Fatal("the owner's message did not reach the runner")
	}
	humanAt := principalInfo(t, manager, target.ID).LastHumanMessageAt
	if humanAt == nil {
		t.Fatal("the owner's message stamped no human contact at all")
	}

	// The provider writes his turn into its transcript, as it does for every
	// message, and then writes its own tick three seconds later.
	spoken := time.UnixMilli(*humanAt)
	recordProviderTranscript(t, manager, target.ID, userTranscriptRecord(spoken, "?"))
	tickAt := spoken.Add(3 * time.Second)
	recordProviderTranscript(t, manager, target.ID,
		userTranscriptRecord(tickAt, "INTEGRATOR TICK (founder-ordered 30-min cadence)"))

	info := principalInfo(t, manager, target.ID)
	if info.LastUserMessageAt == nil || *info.LastUserMessageAt != tickAt.UnixMilli() {
		t.Fatalf("LastUserMessageAt = %v, want the tick at %d", info.LastUserMessageAt, tickAt.UnixMilli())
	}
	if info.LastHumanMessageAt == nil || *info.LastHumanMessageAt != *humanAt {
		t.Fatalf("LastHumanMessageAt = %v, want the owner's message at %d. "+
			"The tick landed %s after he spoke and moved the only number the product had for "+
			"human contact; that is precisely what buried his question.",
			describeMillis(info.LastHumanMessageAt), *humanAt, tickAt.Sub(spoken))
	}
	if info.LastAgentMessageAt != nil {
		t.Errorf("LastAgentMessageAt = %d; no other session sent anything here",
			*info.LastAgentMessageAt)
	}
}

// A stamp nobody wrote down is a stamp the next daemon restart loses, and the
// question it answers — has a person ever spoken into this — is asked most
// often about sessions that have been around long enough to outlive a daemon.
func TestAHumanMessageReachesTheRunnerMetadataDocument(t *testing.T) {
	manager, _, root := newPrincipalManager(t)
	target, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: root, Name: "Workbench",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !manager.Input(context.Background(), target.ID, "still here?") {
		t.Fatal("Input did not reach the runner")
	}
	live := principalInfo(t, manager, target.ID)

	path := filepath.Join(testConfig(root).RunnerStateDir, target.ID+".json")
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read runner metadata: %v", err)
	}
	var document struct {
		LastHumanMessageAt *int64 `json:"last_human_message_at"`
		LastAgentMessageAt *int64 `json:"last_agent_message_at"`
	}
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	if document.LastHumanMessageAt == nil || *document.LastHumanMessageAt != *live.LastHumanMessageAt {
		t.Fatalf("persisted last_human_message_at = %s, want the live stamp %d; "+
			"document = %s", describeMillis(document.LastHumanMessageAt),
			*live.LastHumanMessageAt, encoded)
	}
	if document.LastAgentMessageAt != nil {
		t.Errorf("persisted last_agent_message_at = %d, want absent", *document.LastAgentMessageAt)
	}
}

func describeMillis(value *int64) string {
	if value == nil {
		return "unset"
	}
	return fmt.Sprintf("%d", *value)
}
