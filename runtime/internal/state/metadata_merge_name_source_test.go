package state

import (
	"path/filepath"
	"testing"
)

// Where a name came from is daemon-owned in exactly the way the name itself
// is, and it has to survive a runner write for the same reason: the runner
// rebuilds its document from launch configuration, so it carries no
// NameSource at all and would write the field back to empty.
//
// Empty means "adoptable". Losing an explicit source therefore does not lose a
// name -- the name is preserved beside it -- it silently un-pins the name the
// user chose, and the next provider title overwrites it. The failure needs no
// concurrency and shows up nowhere until the title changes, at which point the
// user's rename is simply gone.
func TestARunnerWriteKeepsTheSessionPinnedToTheNameTheUserChose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "11111111-2222-4333-8444-555555555555.json")
	id := "11111111-2222-4333-8444-555555555555"

	// The daemon launched the runner with an auto-name, then the provider
	// title was adopted, then the user renamed the session by hand.
	if err := WriteMetadata(path, Metadata{ID: id, Name: "Claude - Projects"}); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}
	adopted := readMetadataForMerge(path)
	adopted.Name = "TexasT"
	adopted.NameSource = NameSourceProvider
	if err := WriteMetadata(path, adopted); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	renamed := readMetadataForMerge(path)
	renamed.Name = "Texas billing"
	renamed.NameSource = NameSourceExplicit
	if err := WriteMetadata(path, renamed); err != nil {
		t.Fatalf("rename: %v", err)
	}

	// What the runner writes next: rebuilt from its launch configuration, so
	// it carries the launch name and no name source whatsoever.
	if err := WriteRunnerMetadata(path, Metadata{ID: id, Name: "Claude - Projects", PID: 4242}); err != nil {
		t.Fatalf("runner write: %v", err)
	}

	final := readMetadataForMerge(path)
	if final.Name != "Texas billing" {
		t.Errorf("name = %q, want %q", final.Name, "Texas billing")
	}
	if final.NameSource != NameSourceExplicit {
		t.Errorf("name source = %q, want %q — an ordinary runner write un-pinned the name "+
			"the user chose, so the next title Claude generates silently replaces it",
			final.NameSource, NameSourceExplicit)
	}
	if final.PID != 4242 {
		t.Errorf("pid = %d, want the runner's own field to still win at 4242", final.PID)
	}
}

// The adoptable sources have to survive too, or a session that is following
// the provider's title looks like a fresh launch after every runner write.
// That is not visible on its own -- both values are adoptable -- but it is the
// same field, and only preserving one of its three values is an accident
// waiting for a fourth.
func TestARunnerWriteKeepsAnAdoptedProviderTitleAndItsSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "11111111-2222-4333-8444-555555555555.json")
	id := "11111111-2222-4333-8444-555555555555"
	if err := WriteMetadata(path, Metadata{
		ID: id, Name: "TexasT", NameSource: NameSourceProvider,
	}); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}
	if err := WriteRunnerMetadata(path, Metadata{ID: id, Name: "Claude - Projects"}); err != nil {
		t.Fatalf("runner write: %v", err)
	}
	final := readMetadataForMerge(path)
	if final.Name != "TexasT" || final.NameSource != NameSourceProvider {
		t.Errorf("name/source = %q/%q, want %q/%q", final.Name, final.NameSource, "TexasT", NameSourceProvider)
	}
}
