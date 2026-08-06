package backup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

// Resolve is the single funnel that cat, source, search, and usage all reach
// the conversation through, so this is the test that decides whether "keep all
// the context of the work you did on your machine" is true after the provider
// prunes its own transcript on a 30-day timer.
func TestResolvePrefersProviderAndFallsBackToTheMirror(t *testing.T) {
	stateDir := t.TempDir()
	projects := t.TempDir()
	const id = "0f2c1a44-9b77-4a2e-9c31-6d5a8e77b012"

	cwd := "/Users/someone/work"
	bucket := filepath.Join(projects, watch.EncodeClaudeCWD(cwd))
	if err := os.MkdirAll(bucket, 0o755); err != nil {
		t.Fatal(err)
	}
	providerPath := filepath.Join(bucket, id+".jsonl")
	line := `{"type":"user","uuid":"u1","sessionId":"` + id + `",` +
		`"message":{"role":"user","content":"the auth fix we discussed"}}` + "\n"
	if err := os.WriteFile(providerPath, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	mirrorPath := watch.TranscriptMirrorPath(stateDir, id)
	if err := os.WriteFile(mirrorPath, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	resolver := Resolver{ClaudeProjectsDir: projects, RunnerStateDir: stateDir}
	session := Session{ID: id, Tool: "claude", Command: "claude", CWD: cwd}

	// While the provider still has it, the provider file wins. That is what
	// keeps a session from resolving to two transcripts and being counted
	// twice by usage.
	path, tool := resolver.Resolve(session)
	if tool != "claude" || path != providerPath {
		t.Fatalf("Resolve = (%q, %q), want the provider file %q", path, tool, providerPath)
	}

	// The provider prunes on its own schedule. On this machine that has
	// already removed 87% of recorded conversations.
	if err := os.Remove(providerPath); err != nil {
		t.Fatal(err)
	}
	path, tool = resolver.Resolve(session)
	if tool != "claude" {
		t.Fatalf("tool = %q after pruning, want claude", tool)
	}
	if path != mirrorPath {
		t.Fatalf("Resolve = %q after the provider pruned its transcript, want the mirror %q — "+
			"an empty path is the missing/0-bytes state this exists to prevent", path, mirrorPath)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the mirror did not read back: %v", err)
	}
	if string(body) != line {
		t.Fatalf("mirror content = %q, want the provider records verbatim", string(body))
	}
}

// With no runner state dir the fallback is inert, so nothing changes for
// callers that never opted in.
func TestResolveWithoutARunnerStateDirIsUnchanged(t *testing.T) {
	projects := t.TempDir()
	const id = "0f2c1a44-9b77-4a2e-9c31-6d5a8e77b013"
	resolver := Resolver{ClaudeProjectsDir: projects}
	path, _ := resolver.Resolve(Session{ID: id, Tool: "claude", Command: "claude", CWD: "/nowhere"})
	if path != "" {
		t.Fatalf("Resolve = %q, want no path when neither provider nor mirror exists", path)
	}
}
