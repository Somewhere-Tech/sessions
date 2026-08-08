package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The message-principal clocks are daemon-owned in exactly the way the pin is:
// stamped at Sessions' own input boundary, absent from anything a runner knows
// about, and therefore dropped by every ordinary runner write unless the merge
// preserves them.
//
// The loss is invisible while the daemon is up and appears at the next restart,
// where it reads as a session no human has ever spoken into — which is the one
// answer these fields exist to get right.
func TestARunnerWriteKeepsTheMessagePrincipalClocks(t *testing.T) {
	const id = "77777777-8888-4999-8aaa-cccccccccccc"
	path := filepath.Join(t.TempDir(), id+".json")

	if err := WriteMetadata(path, Metadata{ID: id, Name: "integrator"}); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}
	humanAt, agentAt := int64(1786000000000), int64(1786000500000)
	stamped := readMetadataForMerge(path)
	stamped.LastHumanMessageAt = &humanAt
	stamped.LastAgentMessageAt = &agentAt
	if err := WriteMetadata(path, stamped); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	// What the runner writes next, rebuilt from its launch configuration and
	// carrying neither clock.
	fromRunner := Metadata{ID: id, Name: "integrator", PID: 4711}
	if err := WriteRunnerMetadata(path, fromRunner); err != nil {
		t.Fatalf("runner write: %v", err)
	}

	final := readMetadataForMerge(path)
	if final.LastHumanMessageAt == nil || *final.LastHumanMessageAt != humanAt {
		t.Errorf("last_human_message_at = %v, want it preserved at %d — an ordinary runner "+
			"write discarded the only durable record that a person had spoken into this "+
			"session, so after the next daemon restart it reads as machine-only work",
			final.LastHumanMessageAt, humanAt)
	}
	if final.LastAgentMessageAt == nil || *final.LastAgentMessageAt != agentAt {
		t.Errorf("last_agent_message_at = %v, want it preserved at %d",
			final.LastAgentMessageAt, agentAt)
	}
	if final.PID != 4711 {
		t.Errorf("pid = %d, want the runner's own field to still win at 4711", final.PID)
	}
}

// A runner document that somehow carries its own clocks is a stale launch-time
// view. The daemon is the only writer, so an absent value on disk means nobody
// of that kind has spoken and must not be overwritten by the runner's guess.
func TestARunnerWriteCannotInventMessagePrincipalClocks(t *testing.T) {
	const id = "77777777-8888-4999-8aaa-dddddddddddd"
	path := filepath.Join(t.TempDir(), id+".json")
	if err := WriteMetadata(path, Metadata{ID: id}); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}
	invented := int64(1786000000000)
	fromRunner := Metadata{ID: id, PID: 12, LastHumanMessageAt: &invented}
	if err := WriteRunnerMetadata(path, fromRunner); err != nil {
		t.Fatalf("runner write: %v", err)
	}
	if got := readMetadataForMerge(path).LastHumanMessageAt; got != nil {
		t.Errorf("last_human_message_at = %d, want absent", *got)
	}
}

// The clocks have to reach disk in the first place, and come back on the way in.
func TestMessagePrincipalClocksRoundTripThroughRunnerMetadata(t *testing.T) {
	const id = "77777777-8888-4999-8aaa-eeeeeeeeeeee"
	path := filepath.Join(t.TempDir(), id+".json")
	humanAt, agentAt := int64(1786000000000), int64(1786000500000)
	if err := WriteMetadata(path, Metadata{
		ID: id, LastHumanMessageAt: &humanAt, LastAgentMessageAt: &agentAt,
	}); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	if document["last_human_message_at"] == nil || document["last_agent_message_at"] == nil {
		t.Fatalf("runner metadata document = %s, want both clocks persisted", encoded)
	}
	restored, err := readRunnerMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if restored.LastHumanMessageAt == nil || *restored.LastHumanMessageAt != humanAt ||
		restored.LastAgentMessageAt == nil || *restored.LastAgentMessageAt != agentAt {
		t.Fatalf("restored clocks = %v/%v, want %d/%d",
			restored.LastHumanMessageAt, restored.LastAgentMessageAt, humanAt, agentAt)
	}
	// An older document that never carried them means "nobody has spoken yet",
	// not the epoch.
	empty := filepath.Join(t.TempDir(), id+".json")
	if err := WriteMetadata(empty, Metadata{ID: id}); err != nil {
		t.Fatal(err)
	}
	older, err := readRunnerMetadata(empty)
	if err != nil {
		t.Fatal(err)
	}
	if older.LastHumanMessageAt != nil || older.LastAgentMessageAt != nil {
		t.Fatalf("absent clocks decoded as %v/%v, want nil",
			older.LastHumanMessageAt, older.LastAgentMessageAt)
	}
}

func TestMessagePrincipalStampsOnlyMoveForward(t *testing.T) {
	session := &Session{}
	later := time.Now().UnixMilli()
	if !session.noteMessagePrincipal(PrincipalHuman, later) {
		t.Fatal("first stamp should persist")
	}
	if session.noteMessagePrincipal(PrincipalHuman, later-5000) {
		t.Error("an older stamp asked to be persisted")
	}
	if got := session.Info().LastHumanMessageAt; got == nil || *got != later {
		t.Fatalf("LastHumanMessageAt = %v, want it held at %d", got, later)
	}
	// Within the coalescing window a stamp still moves in memory; it just does
	// not pay a metadata write for every keystroke.
	if session.noteMessagePrincipal(PrincipalHuman, later+1) {
		t.Error("a stamp inside the coalescing window asked to be persisted")
	}
	if got := session.Info().LastHumanMessageAt; got == nil || *got != later+1 {
		t.Fatalf("LastHumanMessageAt = %v, want %d", got, later+1)
	}
	if !session.noteMessagePrincipal(PrincipalHuman,
		later+messagePrincipalPersistInterval.Milliseconds()) {
		t.Error("a stamp past the coalescing window did not ask to be persisted")
	}
}

func TestWhitespaceOnlyInputCarriesNoMessage(t *testing.T) {
	for _, data := range []string{"", " ", "\r", "\n", " \t\r\n"} {
		if carriesMessageText(data) {
			t.Errorf("carriesMessageText(%q) = true, want false", data)
		}
	}
	for _, data := range []string{"?", "hello", " hello "} {
		if !carriesMessageText(data) {
			t.Errorf("carriesMessageText(%q) = false, want true", data)
		}
	}
}
