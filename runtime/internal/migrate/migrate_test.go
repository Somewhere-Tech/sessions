package migrate

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

func TestResolveAndReceiveClaudeConversationIdempotently(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sourceHome := filepath.Join(root, "source-home")
	targetHome := filepath.Join(root, "target-home")
	cwd := filepath.Join(root, "workspace")
	for _, dir := range []string{sourceHome, targetHome, cwd} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", sourceHome)
	provider := "11111111-1111-4111-8111-111111111111"
	conversation := []byte(`{"type":"user","sessionId":"` + provider + `","message":{"content":"move marker"}}` + "\n")
	sourcePath := filepath.Join(sourceHome, ".claude", "projects", watch.EncodeClaudeCWD(cwd), provider+".jsonl")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, conversation, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := ledger.Open(ctx, ledger.Options{Path: filepath.Join(root, "ledger", "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	laneID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	resume := []string{"claude", "--resume", provider}
	if err := store.Boundaries().RecordCreated(ctx, ledger.Created{
		Meta: ledger.Meta{LaneID: laneID}, Tool: "claude-code", Cwd: cwd,
		ResumeArgv: resume, LaneUUID: laneID, ProviderUUID: provider,
		CreatorKind: ledger.CreatorExternal, CreatorID: "migrate-test",
	}); err != nil {
		t.Fatal(err)
	}
	request, err := ResolveSource(ctx, store, SourceSession{
		ID: laneID, Tool: "claude-code", Cmd: "claude", Args: []string{"--resume", provider}, Cwd: cwd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(request.ConversationBytes) != string(conversation) || request.UUID != provider {
		t.Fatalf("resolved request = %#v", request)
	}
	first, err := Receive(ctx, request, ReceiveOptions{Home: targetHome})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Receive(ctx, request, ReceiveOptions{Home: targetHome})
	if err != nil {
		t.Fatal(err)
	}
	if first.AlreadyPresent || !second.AlreadyPresent {
		t.Fatalf("idempotence first=%#v second=%#v", first, second)
	}
	got, err := os.ReadFile(first.ConversationPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(conversation) {
		t.Fatalf("target conversation = %q", got)
	}
	request.ConversationBytes = append([]byte(nil), conversation...)
	request.ConversationBytes[0] = 'x'
	if _, err := Receive(ctx, request, ReceiveOptions{Home: targetHome}); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("different overwrite error = %v", err)
	}
}

func TestReceiveCodexUsesSessionDateAndUUID(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	provider := "22222222-2222-4222-8222-222222222222"
	line, err := json.Marshal(map[string]any{
		"type": "session_meta", "payload": map[string]any{
			"id": provider, "cwd": cwd, "timestamp": "2026-07-15T23:45:00-07:00",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Receive(context.Background(), ReceiveRequest{
		Tool: "codex", UUID: provider, Cwd: cwd, ConversationBytes: append(line, '\n'),
		ResumeRecipe: []string{"codex", "resume", provider},
	}, ReceiveOptions{Home: root})
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(root, ".codex", "sessions", "2026", "07", "15")
	if filepath.Dir(result.ConversationPath) != wantDir || !strings.Contains(filepath.Base(result.ConversationPath), provider) {
		t.Fatalf("conversation path = %s, want date dir %s and uuid", result.ConversationPath, wantDir)
	}
}

func TestPrepareDirtyWorkspacePushesSnapshotWithoutChangingWorktree(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	worktree := filepath.Join(root, "worktree")
	runGitTest(t, root, "init", "--bare", remote)
	runGitTest(t, root, "init", worktree)
	runGitTest(t, worktree, "config", "user.name", "Move Test")
	runGitTest(t, worktree, "config", "user.email", "move@test.invalid")
	tracked := filepath.Join(worktree, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("pushed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, worktree, "add", "tracked.txt")
	runGitTest(t, worktree, "commit", "-m", "initial")
	runGitTest(t, worktree, "branch", "-M", "main")
	runGitTest(t, worktree, "remote", "add", "origin", remote)
	runGitTest(t, worktree, "push", "-u", "origin", "main")
	clean, err := PrepareWorkspace(ctx, worktree, "clean-session", WorkspaceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !clean.Git || clean.CheckpointRef != "" || clean.Dirty {
		t.Fatalf("clean workspace = %#v", clean)
	}
	if err := os.WriteFile(tracked, []byte("dirty tracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "untracked.txt"), []byte("dirty untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixedNow := func() time.Time { return time.UnixMilli(1_721_234_567_890) }
	dirty, err := PrepareWorkspace(ctx, worktree, "dirty/session", WorkspaceOptions{AllowDirty: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if !dirty.Dirty || dirty.CheckpointRef == "" || dirty.Revision == clean.Revision {
		t.Fatalf("dirty workspace = %#v", dirty)
	}
	if got := runGitTest(t, worktree, "status", "--porcelain=v1"); !strings.Contains(got, "M tracked.txt") || !strings.Contains(got, "?? untracked.txt") {
		t.Fatalf("source worktree changed after checkpoint: %q", got)
	}
	if got := runGitTest(t, worktree, "show", dirty.Revision+":tracked.txt"); got != "dirty tracked" {
		t.Fatalf("checkpoint tracked content = %q", got)
	}
	if got := runGitTest(t, worktree, "show", dirty.Revision+":untracked.txt"); got != "dirty untracked" {
		t.Fatalf("checkpoint untracked content = %q", got)
	}
	remoteLine := runGitTest(t, worktree, "ls-remote", "origin", dirty.CheckpointRef)
	if !strings.HasPrefix(remoteLine, dirty.Revision+"\t") {
		t.Fatalf("remote checkpoint = %q, want revision %s", remoteLine, dirty.Revision)
	}
}

func TestMigrationLedgerEventsAreAdditive(t *testing.T) {
	ctx := context.Background()
	store, err := ledger.Open(ctx, ledger.Options{Path: filepath.Join(t.TempDir(), "ledger.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrations().RecordMovedTo(ctx, ledger.MovedTo{
		Meta: ledger.Meta{LaneID: "source"}, TargetEndpoint: "http://127.0.0.1:9999", NewLaneID: "target",
	}); err != nil {
		t.Fatal(err)
	}
	events, err := store.Events(ctx, "source")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != ledger.EventMovedTo || events[0].Actor != ledger.ActorUser {
		t.Fatalf("events = %#v", events)
	}
	if err := store.Migrations().RecordMovedFrom(ctx, ledger.MovedFrom{
		Meta: ledger.Meta{LaneID: "target"}, SourceEndpoint: "this-machine", SourceLaneID: "source",
	}); err != nil {
		t.Fatal(err)
	}
	targetEvents, err := store.Events(ctx, "target")
	if err != nil {
		t.Fatal(err)
	}
	states := ledger.Fold(targetEvents)
	var target ledger.LaneState
	for _, state := range states {
		if state.LaneID == "target" {
			target = state
		}
	}
	if target.MovedFromMachine != "this-machine" || target.MovedFromLaneID != "source" {
		t.Fatalf("target provenance = %#v", target)
	}
}

func runGitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	encoded, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, encoded)
	}
	return strings.TrimSpace(string(encoded))
}
