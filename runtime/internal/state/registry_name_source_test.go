package state

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
)

// The user's session was called "TexasT" in Claude's own conversation list,
// because Claude titles conversations. Sessions called the same conversation
// "Claude - Projects" -- its launch-time auto-name -- forever, so searching
// Sessions for the name every Claude surface showed found nothing at all.
func nameSourceRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	root := t.TempDir()
	registry := NewRegistry(Config{
		DefaultShell: "/bin/bash", DefaultCwd: root, DefaultCols: 300, DefaultRows: 50,
		RunnerStateDir: filepath.Join(root, "runners"), LaunchAgentsDir: filepath.Join(root, "agents"),
	}, prototest.NewLauncher())
	created, err := registry.Create(context.Background(), CreateSessionRequest{
		Cmd: "claude", Cwd: root, Name: "Claude - Projects",
	})
	if err != nil {
		t.Fatal(err)
	}
	return registry, created.ID
}

func storedName(t *testing.T, registry *Registry, id string) (string, string) {
	t.Helper()
	encoded, err := os.ReadFile(filepath.Join(registry.Config().RunnerStateDir, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata Metadata
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		t.Fatal(err)
	}
	return metadata.Name, metadata.NameSource
}

func liveName(t *testing.T, registry *Registry, id string) (string, string) {
	t.Helper()
	session, ok := registry.Get(id)
	if !ok {
		t.Fatalf("session %s is not live", id)
	}
	info := session.Info()
	return info.Name, info.NameSource
}

func TestTheSessionCardFollowsTheTitleClaudeGivesTheConversation(t *testing.T) {
	registry, id := nameSourceRegistry(t)

	adopted, err := registry.AdoptProviderTitle(id, "TexasT")
	if err != nil {
		t.Fatal(err)
	}
	if !adopted {
		t.Fatal("a launch-named session refused the provider's own conversation title")
	}
	if name, source := liveName(t, registry, id); name != "TexasT" || source != NameSourceProvider {
		t.Errorf("live name/source = %q/%q, want %q/%q", name, source, "TexasT", NameSourceProvider)
	}
	if name, source := storedName(t, registry, id); name != "TexasT" || source != NameSourceProvider {
		t.Errorf("stored name/source = %q/%q, want %q/%q", name, source, "TexasT", NameSourceProvider)
	}
}

// A title Claude has already given the session is not re-written on every
// event, and a title Claude changes is followed. Both matter: the first keeps
// the metadata file from being rewritten on every transcript record, the
// second is what keeps the two surfaces in agreement over a session's life.
func TestARetitledConversationRenamesTheSessionAgain(t *testing.T) {
	registry, id := nameSourceRegistry(t)
	if _, err := registry.AdoptProviderTitle(id, "TexasT"); err != nil {
		t.Fatal(err)
	}

	repeated, err := registry.AdoptProviderTitle(id, "TexasT")
	if err != nil {
		t.Fatal(err)
	}
	if repeated {
		t.Error("the same title was adopted twice, rewriting metadata for no change")
	}

	changed, err := registry.AdoptProviderTitle(id, "Texas rate tables")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("Claude retitled the conversation and the session kept the old name")
	}
	if name, _ := liveName(t, registry, id); name != "Texas rate tables" {
		t.Errorf("live name = %q, want %q", name, "Texas rate tables")
	}
	if name, _ := storedName(t, registry, id); name != "Texas rate tables" {
		t.Errorf("stored name = %q, want %q", name, "Texas rate tables")
	}
}

// The guard the whole feature rests on. Following the provider is only safe
// because a name a person chose is never overwritten by it; without this the
// feature takes names away from users instead of giving them one.
func TestAnExplicitRenameIsNeverOverwrittenByAProviderTitle(t *testing.T) {
	registry, id := nameSourceRegistry(t)
	if _, err := registry.UpdateName(id, "Texas billing"); err != nil {
		t.Fatal(err)
	}
	if _, source := liveName(t, registry, id); source != NameSourceExplicit {
		t.Fatalf("rename left name source %q, want %q", source, NameSourceExplicit)
	}

	adopted, err := registry.AdoptProviderTitle(id, "TexasT")
	if err != nil {
		t.Fatal(err)
	}
	if adopted {
		t.Error("the provider's title overwrote a name the user chose")
	}
	if name, source := liveName(t, registry, id); name != "Texas billing" || source != NameSourceExplicit {
		t.Errorf("live name/source = %q/%q, want %q/%q — the user renamed this session and "+
			"Claude retitling its conversation took the name away again",
			name, source, "Texas billing", NameSourceExplicit)
	}
	if name, source := storedName(t, registry, id); name != "Texas billing" || source != NameSourceExplicit {
		t.Errorf("stored name/source = %q/%q, want %q/%q", name, source, "Texas billing", NameSourceExplicit)
	}
}

// The same guard for a session the daemon is not holding in memory. The
// on-disk document is the one that survives a restart, so a rename made before
// the daemon went down has to keep protecting the name after it comes back.
func TestAnExplicitRenameSurvivesWhenTheSessionIsNotLive(t *testing.T) {
	root := t.TempDir()
	config := Config{
		DefaultShell: "/bin/bash", DefaultCwd: root, DefaultCols: 300, DefaultRows: 50,
		RunnerStateDir: filepath.Join(root, "runners"), LaunchAgentsDir: filepath.Join(root, "agents"),
	}
	registry := NewRegistry(config, prototest.NewLauncher())
	id := "11111111-2222-4333-8444-555555555555"
	if err := os.MkdirAll(config.RunnerStateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(config.RunnerStateDir, id+".json")
	if err := WriteMetadata(path, Metadata{
		ID: id, Name: "Texas billing", NameSource: NameSourceExplicit, Cmd: "claude", Cwd: root,
	}); err != nil {
		t.Fatal(err)
	}

	adopted, err := registry.AdoptProviderTitle(id, "TexasT")
	if err != nil {
		t.Fatal(err)
	}
	if adopted {
		t.Error("a provider title overwrote a rename recorded only on disk")
	}
	if name, source := storedName(t, registry, id); name != "Texas billing" || source != NameSourceExplicit {
		t.Errorf("stored name/source = %q/%q, want %q/%q", name, source, "Texas billing", NameSourceExplicit)
	}
}

func TestRenameAutoLetsTheProviderTitleFollowAgain(t *testing.T) {
	registry, id := nameSourceRegistry(t)
	if _, err := registry.UpdateName(id, "Texas billing"); err != nil {
		t.Fatal(err)
	}

	released, err := registry.ReleaseName(id)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing has titled this conversation yet, so releasing changes no name.
	if released != "Texas billing" {
		t.Errorf("released name = %q, want the current name %q kept until a title arrives",
			released, "Texas billing")
	}
	if _, source := liveName(t, registry, id); source != NameSourceLaunch {
		t.Errorf("name source after --auto = %q, want %q", source, NameSourceLaunch)
	}

	adopted, err := registry.AdoptProviderTitle(id, "TexasT")
	if err != nil {
		t.Fatal(err)
	}
	if !adopted {
		t.Fatal("--auto did not restore following the provider's title")
	}
	if name, source := liveName(t, registry, id); name != "TexasT" || source != NameSourceProvider {
		t.Errorf("live name/source = %q/%q, want %q/%q", name, source, "TexasT", NameSourceProvider)
	}
}

// --auto on a conversation Claude has already titled should not wait for the
// next title record, which for a finished conversation may never come.
func TestRenameAutoAdoptsATitleTheConversationAlreadyHas(t *testing.T) {
	registry, id := nameSourceRegistry(t)
	if _, err := registry.UpdateName(id, "Texas billing"); err != nil {
		t.Fatal(err)
	}
	session, ok := registry.Get(id)
	if !ok {
		t.Fatal("session is not live")
	}
	session.RecordClaudeEvent(json.RawMessage(`{"type":"ai-title","aiTitle":"TexasT"}`))

	released, err := registry.ReleaseName(id)
	if err != nil {
		t.Fatal(err)
	}
	if released != "TexasT" {
		t.Errorf("released name = %q, want the title Claude already gave it, %q", released, "TexasT")
	}
	if name, source := storedName(t, registry, id); name != "TexasT" || source != NameSourceProvider {
		t.Errorf("stored name/source = %q/%q, want %q/%q", name, source, "TexasT", NameSourceProvider)
	}
}

// A conversation with no title is the normal state for the first minute of
// every session. Adopting emptiness would erase the launch name and leave a
// card with nothing on it.
func TestAnEmptyProviderTitleIsNeverAdopted(t *testing.T) {
	registry, id := nameSourceRegistry(t)
	for _, title := range []string{"", "   ", "\t\n", "\x01\x02"} {
		adopted, err := registry.AdoptProviderTitle(id, title)
		if err != nil {
			t.Fatalf("AdoptProviderTitle(%q) error = %v, want a quiet refusal", title, err)
		}
		if adopted {
			t.Errorf("AdoptProviderTitle(%q) adopted a title that is not a name", title)
		}
		if name, source := liveName(t, registry, id); name != "Claude - Projects" || source != "" {
			t.Errorf("after %q the name/source = %q/%q, want the launch name untouched", title, name, source)
		}
	}
}

// A session created before NameSource existed has no recorded source. It is
// adoptable, which is what makes the change migration-free.
func TestASessionWithNoRecordedSourceIsAdoptable(t *testing.T) {
	registry, id := nameSourceRegistry(t)
	if _, source := storedName(t, registry, id); source != "" {
		t.Fatalf("a newly created session recorded name source %q, want none", source)
	}
	adopted, err := registry.AdoptProviderTitle(id, "TexasT")
	if err != nil {
		t.Fatal(err)
	}
	if !adopted {
		t.Fatal("a session with no recorded name source refused the provider's title")
	}
}

// Adoption uses the same name rule a person's rename uses, so a title long
// enough to be a paragraph still lands as a label rather than being dropped
// for exceeding it.
func TestAnOverlongProviderTitleIsBoundedRatherThanRefused(t *testing.T) {
	registry, id := nameSourceRegistry(t)
	adopted, err := registry.AdoptProviderTitle(id, strings.Repeat("Texas ", 60))
	if err != nil {
		t.Fatal(err)
	}
	if !adopted {
		t.Fatal("a long provider title was refused instead of bounded")
	}
	name, _ := liveName(t, registry, id)
	if count := len([]rune(name)); count > 120 {
		t.Errorf("adopted name is %d runes, past the %d-rune session name rule", count, 120)
	}
	if _, err := validateSessionName(name); err != nil {
		t.Errorf("adopted name is not a legal session name: %v", err)
	}
}
