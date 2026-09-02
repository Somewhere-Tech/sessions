package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestApplyInheritedPermissionsPreservesProviderSpecificMode(t *testing.T) {
	tests := []struct {
		name   string
		tool   state.SessionTool
		child  []string
		parent []string
		want   []string
	}{
		{
			name: "Claude plan", tool: state.ToolClaude,
			child:  []string{"--permission-mode", "manual", "--model", "sonnet"},
			parent: []string{"--permission-mode", "plan", "--session-id", "parent"},
			want:   []string{"--permission-mode", "plan", "--model", "sonnet"},
		},
		{
			name: "Codex sandbox", tool: state.ToolCodex,
			child:  []string{"--sandbox", "workspace-write", "--ask-for-approval", "on-request", "--model", "gpt"},
			parent: []string{"--sandbox", "read-only", "--ask-for-approval", "untrusted"},
			want:   []string{"--sandbox", "read-only", "--ask-for-approval", "untrusted", "--model", "gpt"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := applyInheritedPermissions(test.tool, test.child, test.parent, state.PermissionsConstrained)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("applyInheritedPermissions() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestConstrainedResolutionDoesNotEraseClaudePermissionMode(t *testing.T) {
	args := []string{"--permission-mode", "acceptEdits", "--model", "opus"}
	if got := applyResolvedPermissions(state.ToolClaude, args, state.PermissionsConstrained); !reflect.DeepEqual(got, args) {
		t.Fatalf("applyResolvedPermissions() = %#v, want %#v", got, args)
	}
}

// A lane created by another session works in its own Sessions-owned worktree
// by default; a folder that cannot host one is shared instead of failing, and
// --no-worktree declines the default outright.
func TestDelegatedLaneDefaultsToItsOwnWorktree(t *testing.T) {
	root := t.TempDir()
	manager, _, _ := newWorktreeTestManager(t, root)
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		command := exec.Command("git", args...)
		command.Dir = repo
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	parent, err := manager.Create(context.Background(), state.CreateSessionRequest{Cmd: "/bin/sh", Cwd: repo, Name: "manager"})
	if err != nil {
		t.Fatal(err)
	}
	lane, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: repo, Name: "marketing page", CreatorSessionID: parent.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if lane.WorktreePath == "" || lane.Branch == "" || lane.Cwd != lane.WorktreePath || lane.SourceRepo == "" {
		t.Fatalf("delegated lane did not get a worktree: %#v", lane)
	}
	if lane.Cwd == repo {
		t.Fatal("lane shares the manager's checkout")
	}

	shared, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: repo, Name: "shared", CreatorSessionID: parent.ID, NoWorktree: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if shared.WorktreePath != "" || shared.Cwd != repo {
		t.Fatalf("--no-worktree lane still got a worktree: %#v", shared)
	}

	plain := filepath.Join(root, "notes")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	fallback, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: plain, Name: "notes lane", CreatorSessionID: parent.ID,
	})
	if err != nil {
		t.Fatalf("a folder that cannot host a worktree must be shared, not fail: %v", err)
	}
	if fallback.WorktreePath != "" || fallback.Cwd != plain {
		t.Fatalf("plain-folder lane = %#v, want shared folder", fallback)
	}

	// A person's own session in the same repository is untouched by the default.
	direct, err := manager.Create(context.Background(), state.CreateSessionRequest{Cmd: "/bin/sh", Cwd: repo, Name: "mine"})
	if err != nil {
		t.Fatal(err)
	}
	if direct.WorktreePath != "" {
		t.Fatalf("user session got a worktree by default: %#v", direct)
	}
}
