package session

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// A command lane is not an interactive Claude session. In particular,
// adding --remote-control to a print command can prevent useful output.
func TestClaudeCommandLaneDoesNotInheritInteractiveDefaults(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	settings := state.ClaudeSettings{
		RemoteControl: state.ClaudeChoiceOn, RemoteControlNamePrefix: "sessions",
		PermissionMode: state.ClaudePermissionBypass, Model: "opus", Effort: "high",
		Chrome: state.ClaudeChoiceOn, SomewhereMCP: state.ClaudeSomewhereEnsure,
	}
	if err := state.SaveSettings(settingsPath, settingsWithRemoteControlConsent(settings)); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{config: state.Config{SettingsPath: settingsPath}}
	for _, args := range [][]string{
		{"-p", "Reply once", "--output-format", "json"},
		{"--print", "--resume", "11111111-1111-4111-8111-111111111111", "Reply once"},
		{"auth", "status", "--json"},
	} {
		request := state.CreateSessionRequest{Cmd: "claude", Kind: state.KindLane, Args: args}
		got, err := manager.applyClaudeDefaults(request)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, request) {
			t.Fatalf("command lane changed: got %#v, want %#v", got, request)
		}
	}
}

func TestClaudeCommandLaneRejectsManagedLaunchOptions(t *testing.T) {
	manager := &Manager{}
	_, err := manager.applyClaudeDefaults(state.CreateSessionRequest{
		Cmd: "claude", Kind: state.KindLane,
		Claude: &state.ClaudeSessionOptions{RemoteControl: state.ClaudeChoiceOn},
	})
	if err == nil {
		t.Fatal("command lane silently ignored managed launch options")
	}
}
