package main

import "testing"

func TestProviderConversationIdentityFromLaunchArgs(t *testing.T) {
	const providerID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	tests := []struct {
		name             string
		cfg              config
		wantConversation string
		wantClaude       string
	}{
		{name: "Claude resume", cfg: config{cmd: "claude", args: []string{"--resume", providerID}}, wantConversation: providerID, wantClaude: providerID},
		{name: "Claude short resume", cfg: config{cmd: "/usr/local/bin/claude", args: []string{"-r", providerID}}, wantConversation: providerID, wantClaude: providerID},
		{name: "Claude fresh pinned id", cfg: config{cmd: "claude", args: []string{"--session-id=" + providerID}}, wantConversation: providerID, wantClaude: providerID},
		{name: "Codex resume", cfg: config{cmd: "codex", args: []string{"resume", providerID}}, wantConversation: providerID},
		{name: "Shell", cfg: config{cmd: "/bin/zsh", args: []string{"--resume", providerID}}},
		{name: "Invalid id", cfg: config{cmd: "claude", args: []string{"--resume", "not-a-conversation"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conversation, claude := providerConversationIdentity(test.cfg)
			if conversation != test.wantConversation || claude != test.wantClaude {
				t.Fatalf("providerConversationIdentity() = %q, %q; want %q, %q", conversation, claude, test.wantConversation, test.wantClaude)
			}
		})
	}
}
