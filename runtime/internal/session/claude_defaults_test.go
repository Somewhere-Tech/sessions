package session

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestClaudeDefaultsBecomeLaunchArguments(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settingsPath := filepath.Join(home, "settings.json")
	settings := state.ClaudeSettings{
		RemoteControl: state.ClaudeChoiceOn, PermissionMode: state.ClaudePermissionManual,
		Model: "opus", Effort: "high", Chrome: state.ClaudeChoiceOff,
		SomewhereMCP: state.ClaudeSomewhereEnsure, RemoteControlNamePrefix: "sessions",
	}
	if err := state.SaveSettings(settingsPath, state.Settings{Claude: &settings}); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{config: state.Config{SettingsPath: settingsPath}}
	request, err := manager.applyClaudeDefaults(state.CreateSessionRequest{
		Cmd: "claude", Cwd: home, Args: []string{"--session-id", "fixture"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, sequence := range [][]string{
		{"--permission-mode", "manual"},
		{"--remote-control"},
		{"--remote-control-session-name-prefix", "sessions"},
		{"--model", "opus"},
		{"--effort", "high"},
		{"--no-chrome"},
	} {
		if !containsArgSequence(request.Args, sequence...) {
			t.Errorf("launch args %q missing %q", request.Args, sequence)
		}
	}
	mcpIndex := slices.Index(request.Args, "--mcp-config")
	if mcpIndex < 0 || mcpIndex+1 >= len(request.Args) || !strings.Contains(request.Args[mcpIndex+1], `"command":"somewhere"`) {
		t.Fatalf("launch args missing managed Somewhere MCP: %q", request.Args)
	}
}

func TestClaudeStructuredForcesRemoteControlOff(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	settings := state.ClaudeSettings{
		RemoteControl: state.ClaudeChoiceOn, PermissionMode: state.ClaudeChoiceInherit,
		Effort: state.ClaudeChoiceInherit, Chrome: state.ClaudeChoiceInherit,
		SomewhereMCP: state.ClaudeChoiceInherit, RemoteControlNamePrefix: "sessions",
	}
	if err := state.SaveSettings(settingsPath, state.Settings{Claude: &settings}); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{config: state.Config{SettingsPath: settingsPath}}
	request, err := manager.applyClaudeDefaults(state.CreateSessionRequest{
		Cmd: "claude", Kind: state.KindClaudeStructured,
		Args: []string{"--session-id", "fixture"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if hasAnyArg(request.Args, "--remote-control", "--remote-control-session-name-prefix") {
		t.Fatalf("Rich Claude launch received interactive Remote Control args: %q", request.Args)
	}
	settingsIndex := slices.Index(request.Args, "--settings")
	if settingsIndex < 0 || settingsIndex+1 >= len(request.Args) ||
		!strings.Contains(request.Args[settingsIndex+1], `"remoteControlAtStartup":false`) {
		t.Fatalf("Rich Claude launch did not explicitly disable provider Remote Control: %q", request.Args)
	}

	_, err = manager.applyClaudeDefaults(state.CreateSessionRequest{
		Cmd: "claude", Kind: state.KindClaudeStructured,
		Args: []string{"--session-id", "fixture", "--remote-control"},
	})
	if err == nil || !strings.Contains(err.Error(), "requires a Terminal session") {
		t.Fatalf("explicit Rich Remote Control error = %v", err)
	}
}

func TestClaudePerSessionOverridesAndExistingSomewhereConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"renamed":{"type":"http","url":"https://mcp.somewhere.tech/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(home, "settings.json")
	settings := state.ClaudeSettings{
		RemoteControl: state.ClaudeChoiceOn, PermissionMode: state.ClaudePermissionBypass,
		Model: "opus", Effort: "high", Chrome: state.ClaudeChoiceOn, SomewhereMCP: state.ClaudeSomewhereEnsure,
	}
	if err := state.SaveSettings(settingsPath, state.Settings{Claude: &settings}); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{config: state.Config{SettingsPath: settingsPath}}
	request, err := manager.applyClaudeDefaults(state.CreateSessionRequest{
		Cmd: "claude", Cwd: home, Args: []string{"--session-id", "fixture"},
		Claude: &state.ClaudeSessionOptions{
			RemoteControl: state.ClaudeChoiceInherit, PermissionMode: state.ClaudePermissionPlan,
			Model: "sonnet", Effort: "low", Chrome: state.ClaudeChoiceOff,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if hasAnyArg(request.Args, "--remote-control") || hasAnyArg(request.Args, "--mcp-config") {
		t.Fatalf("per-session inherit/existing MCP were not respected: %q", request.Args)
	}
	for _, sequence := range [][]string{{"--permission-mode", "plan"}, {"--model", "sonnet"}, {"--effort", "low"}, {"--no-chrome"}} {
		if !containsArgSequence(request.Args, sequence...) {
			t.Errorf("launch args %q missing %q", request.Args, sequence)
		}
	}
}

func TestSavedClaudeFullAccessChoiceRemainsExplicitlyHonored(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	settings := state.ClaudeSettings{
		RemoteControl: state.ClaudeChoiceInherit, PermissionMode: state.ClaudePermissionBypass,
		Effort: state.ClaudeChoiceInherit, Chrome: state.ClaudeChoiceInherit,
		SomewhereMCP: state.ClaudeChoiceInherit,
	}
	if err := state.SaveSettings(settingsPath, state.Settings{Claude: &settings}); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{config: state.Config{SettingsPath: settingsPath}}
	request, err := manager.applyClaudeDefaults(state.CreateSessionRequest{
		Cmd: "claude", Args: []string{"--session-id", "fixture"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasAnyArg(request.Args, "--dangerously-skip-permissions") {
		t.Fatalf("saved Full Access choice was not honored: %q", request.Args)
	}
}

func TestClaudeSomewhereMCPConflictFailsClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"somewhere":{"command":"different-server"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{}
	_, err := manager.applyClaudeDefaults(state.CreateSessionRequest{
		Cmd: "claude", Cwd: home,
		Claude: &state.ClaudeSessionOptions{SomewhereMCP: state.ClaudeSomewhereEnsure},
	})
	if err == nil || !strings.Contains(err.Error(), "different target") {
		t.Fatalf("conflicting Somewhere MCP error = %v", err)
	}
}

func TestClaudeSomewhereMCPUsesManagerDefaultWorkspace(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".mcp.json"), []byte(`{"mcpServers":{"workspace-somewhere":{"command":"somewhere","args":["mcp"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{config: state.Config{DefaultCwd: workspace}}
	request, err := manager.applyClaudeDefaults(state.CreateSessionRequest{
		Cmd:    "claude",
		Claude: &state.ClaudeSessionOptions{SomewhereMCP: state.ClaudeSomewhereEnsure},
	})
	if err != nil {
		t.Fatal(err)
	}
	if hasAnyArg(request.Args, "--mcp-config") {
		t.Fatalf("manager default workspace MCP was duplicated: %q", request.Args)
	}
}

func TestClaudeExplicitLaunchArgumentsWinOverDefaults(t *testing.T) {
	settings := state.ClaudeSettings{
		RemoteControl:  state.ClaudeChoiceOff,
		PermissionMode: state.ClaudePermissionBypass,
		Model:          "opus",
	}
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	if err := state.SaveSettings(settingsPath, state.Settings{Claude: &settings}); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{config: state.Config{SettingsPath: settingsPath}}
	request, err := manager.applyClaudeDefaults(state.CreateSessionRequest{
		Cmd:  "claude",
		Args: []string{"--settings", "/tmp/custom-claude-settings.json", "--permission-mode=plan", "--model=sonnet"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(request.Args, []string{"--settings", "/tmp/custom-claude-settings.json", "--permission-mode=plan", "--model=sonnet"}) {
		t.Fatalf("explicit Claude arguments were changed: %q", request.Args)
	}
}

func containsArgSequence(args []string, sequence ...string) bool {
	for index := 0; index+len(sequence) <= len(args); index++ {
		if slices.Equal(args[index:index+len(sequence)], sequence) {
			return true
		}
	}
	return false
}
