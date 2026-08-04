package session

import (
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
