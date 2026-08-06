package watch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The project-directory name is a lossy encoding: Claude folds every
// non-alphanumeric byte to a dash, so a bucket cannot be inverted back into the
// directory that produced it. The scanner used to invert it anyway and print
// the result as the conversation's working directory, which on this machine
// turned /Users/uzair/pretty-PTY into /Users/uzair/pretty/PTY -- a path that
// does not exist, in the one column a person uses to recognise which
// conversation is theirs.
func TestScanResumableConversationsPrefersTheRecordedWorkingDirectory(t *testing.T) {
	home := t.TempDir()
	setFixtureHome(t, home)
	// The real directory has a dash in its name, which the bucket cannot
	// distinguish from a separator.
	real := filepath.Join(home, "pretty-PTY")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	id := "44444444-4444-4444-8444-444444444444"
	path := filepath.Join(home, ".claude", "projects", EncodeClaudeCWD(real), id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	record, err := json.Marshal(map[string]any{
		"type": "user", "cwd": real, "entrypoint": "cli", "version": "2.1.220",
		"promptSource": "typed",
		"message":      map[string]any{"role": "user", "content": "recognise me"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(record, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	conversations := ScanResumableConversations()
	if len(conversations) != 1 {
		t.Fatalf("conversations = %#v", conversations)
	}
	got := conversations[0]
	if got.Cwd != real {
		t.Fatalf("cwd = %q, want the recorded %q", got.Cwd, real)
	}
	if strings.Contains(got.Cwd, "pretty/PTY") {
		t.Fatalf("the bucket name was inverted into a directory that never existed: %q", got.Cwd)
	}
	if got.Surface == nil || got.Surface.Kind != SurfaceClaudeCLI || got.Surface.Actor != ActorUser {
		t.Fatalf("surface = %#v", got.Surface)
	}
}

// When nothing recorded a working directory and the inversion names a directory
// that is not on disk, the honest answer is none. A path the reader can neither
// visit nor recognise is worse than a blank column: it invites them to conclude
// the conversation is not the one they are looking for.
func TestScanResumableConversationsNeverInventsAWorkingDirectory(t *testing.T) {
	home := t.TempDir()
	setFixtureHome(t, home)
	id := "55555555-5555-4555-8555-555555555555"
	path := filepath.Join(home, ".claude", "projects", "-Users-nobody-Code-nested-project", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	contents := `{"type":"user","message":{"role":"user","content":"no cwd anywhere"}}` + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	conversations := ScanResumableConversations()
	if len(conversations) != 1 {
		t.Fatalf("conversations = %#v", conversations)
	}
	if got := conversations[0].Cwd; got != "" {
		t.Fatalf("cwd = %q, want none: the bucket names no directory that exists", got)
	}
}

// The inversion is still allowed when it names a directory that is actually
// there, which is what separates a confirmed reading from a fabricated one.
func TestScanResumableConversationsAcceptsAConfirmedInversion(t *testing.T) {
	home := t.TempDir()
	setFixtureHome(t, home)
	workspace := filepath.Join(home, "plainproject")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	id := "66666666-6666-4666-8666-666666666666"
	path := filepath.Join(home, ".claude", "projects", EncodeClaudeCWD(workspace), id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	contents := `{"type":"user","message":{"role":"user","content":"no cwd recorded"}}` + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	conversations := ScanResumableConversations()
	if len(conversations) != 1 {
		t.Fatalf("conversations = %#v", conversations)
	}
	// The bucket is named after the alias-resolved path, so the inversion
	// produces that spelling. What matters is that it names the same real
	// directory rather than one nobody confirmed.
	got := conversations[0].Cwd
	if got == "" {
		t.Fatal("a bucket whose inversion exists on disk produced no directory")
	}
	resolved, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(got) != filepath.Clean(resolved) {
		t.Fatalf("cwd = %q, want the confirmed %q", got, resolved)
	}
}

// Provenance comes off the session_meta line the scan already parses, so a
// Codex conversation carries its surface without a second read of the file.
func TestScanResumableConversationsCarriesCodexProvenance(t *testing.T) {
	home := t.TempDir()
	setFixtureHome(t, home)
	root := filepath.Join(home, ".codex", "sessions", "2026", "08", "05")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(home, "work")
	meta, err := json.Marshal(map[string]any{
		"timestamp": "2026-08-05T13:33:15Z", "type": "session_meta",
		"payload": map[string]any{
			"id": "019fd3a1-5dcf-7850-8b19-12c6a6ce3f2a", "cwd": cwd,
			"originator": "Codex Desktop", "source": "vscode",
			"thread_source": "automation", "cli_version": "0.104.0",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := json.Marshal(map[string]any{
		"timestamp": "2026-08-05T13:34:00Z", "type": "response_item",
		"payload": map[string]any{
			"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": "scheduled sweep"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "rollout-2026-08-05T13-33-15-019fd3a1.jsonl")
	if err := os.WriteFile(path, []byte(string(meta)+"\n"+string(message)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	conversations := ScanResumableConversations()
	if len(conversations) != 1 {
		t.Fatalf("conversations = %#v", conversations)
	}
	surface := conversations[0].Surface
	if surface == nil || surface.Kind != SurfaceCodexDesktop || surface.Actor != ActorAutomation ||
		surface.Originator != "Codex Desktop" || surface.Source != "vscode" {
		t.Fatalf("surface = %#v", surface)
	}
}
