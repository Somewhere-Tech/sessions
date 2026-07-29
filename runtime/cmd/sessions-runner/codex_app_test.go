package main

import (
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
)

func TestCodexConversationOptionsPreservePermissionChoice(t *testing.T) {
	safe := codexConversationOptions(config{
		cwd:  "/tmp",
		args: []string{"--sandbox", "workspace-write", "--ask-for-approval", "on-request"},
	})
	if safe.Sandbox != codexapp.SandboxWorkspaceWrite || safe.ApprovalPolicy != codexapp.ApprovalOnRequest {
		t.Fatalf("safe permissions = %#v", safe)
	}

	full := codexConversationOptions(config{
		cwd:  "/tmp",
		args: []string{"--dangerously-bypass-approvals-and-sandbox"},
	})
	if full.Sandbox != codexapp.SandboxDangerFullAccess || full.ApprovalPolicy != codexapp.ApprovalNever {
		t.Fatalf("full permissions = %#v", full)
	}
}

func TestCodexInterruptInputDoesNotConfuseBracketedPaste(t *testing.T) {
	for _, value := range []string{"\x1b", "\x03"} {
		if !isCodexInterruptInput(value) {
			t.Fatalf("%q was not recognized as an interrupt", value)
		}
	}
	for _, value := range []string{
		"",
		"hello",
		"\r",
		"\x1b[200~hello\x1b[201~",
		"\x1b[A",
	} {
		if isCodexInterruptInput(value) {
			t.Fatalf("%q was incorrectly recognized as an interrupt", value)
		}
	}
}
