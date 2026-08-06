package backup

import (
	"os"
	"path/filepath"
	"testing"
)

// CollectSessions enumerates the runner state directory directly, so every
// ".json" sidecar Paths writes is a candidate phantom session. A phantom here
// would be backed up, listed, and resolved as if it were real work.
func TestCollectSessionsIgnoresRunnerSidecars(t *testing.T) {
	stateDir := t.TempDir()
	const id = "5c9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4bed"

	metadata := `{"id":"` + id + `","cmd":"claude","cwd":"/tmp","createdAt":1,"cols":80,"rows":24}`
	if err := os.WriteFile(filepath.Join(stateDir, id+".json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, sidecar := range []string{
		id + ".manifest.json",
		id + ".continuation.json",
		id + ".transcript.meta.json",
	} {
		if err := os.WriteFile(filepath.Join(stateDir, sidecar), []byte(`{"version":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	sessions := CollectSessions(nil, stateDir)
	if len(sessions) != 1 {
		ids := make([]string, 0, len(sessions))
		for _, session := range sessions {
			ids = append(ids, session.ID)
		}
		t.Fatalf("CollectSessions returned %d sessions %v, want only the real one", len(sessions), ids)
	}
	if sessions[0].ID != id {
		t.Fatalf("session id = %q, want %q", sessions[0].ID, id)
	}
}
