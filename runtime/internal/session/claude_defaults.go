package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

const somewhereMCPURL = "https://mcp.somewhere.tech/mcp"

func (m *Manager) applyClaudeDefaults(request state.CreateSessionRequest) (state.CreateSessionRequest, error) {
	if state.CommandTool(request.Cmd) != state.ToolClaude {
		if request.Claude != nil {
			return request, errors.New("Claude launch options require the claude command")
		}
		return request, nil
	}

	defaults := state.DefaultClaudeSettings()
	if m.config.SettingsPath != "" {
		settings, err := state.LoadSettings(m.config.SettingsPath)
		if err != nil {
			return request, fmt.Errorf("load Claude defaults: %w", err)
		}
		defaults = settings.EffectiveClaude()
	}
	resolved, err := state.ResolveClaudeSettings(defaults, request.Claude)
	if err != nil {
		return request, err
	}
	args := append([]string(nil), request.Args...)

	if !hasAnyArg(args, "--dangerously-skip-permissions", "--permission-mode") {
		switch resolved.PermissionMode {
		case state.ClaudeChoiceInherit:
		case state.ClaudePermissionBypass:
			args = append(args, "--dangerously-skip-permissions")
		default:
			args = append(args, "--permission-mode", resolved.PermissionMode)
		}
	}

	if !hasAnyArg(args, "--remote-control", "--settings") {
		switch resolved.RemoteControl {
		case state.ClaudeChoiceOn:
			args = append(args, "--remote-control")
		case state.ClaudeChoiceOff:
			encoded, _ := json.Marshal(map[string]bool{"remoteControlAtStartup": false})
			args = append(args, "--settings", string(encoded))
		}
	}
	if resolved.RemoteControl != state.ClaudeChoiceOff && resolved.RemoteControlNamePrefix != "" && !hasAnyArg(args, "--remote-control-session-name-prefix") {
		args = append(args, "--remote-control-session-name-prefix", resolved.RemoteControlNamePrefix)
	}
	if resolved.Model != "" && !hasAnyArg(args, "--model", "-m") {
		args = append(args, "--model", resolved.Model)
	}
	if resolved.Effort != state.ClaudeChoiceInherit && !hasAnyArg(args, "--effort") {
		args = append(args, "--effort", resolved.Effort)
	}
	if !hasAnyArg(args, "--chrome", "--no-chrome") {
		switch resolved.Chrome {
		case state.ClaudeChoiceOn:
			args = append(args, "--chrome")
		case state.ClaudeChoiceOff:
			args = append(args, "--no-chrome")
		}
	}
	if resolved.SomewhereMCP == state.ClaudeSomewhereEnsure && !hasAnyArg(args, "--mcp-config") {
		cwd := strings.TrimSpace(request.Cwd)
		if cwd == "" {
			cwd = m.config.DefaultCwd
		}
		configured, err := configuredSomewhereMCP(request.ConfigDir, cwd)
		if err != nil {
			return request, err
		}
		if !configured {
			definition := map[string]any{
				"mcpServers": map[string]any{
					"somewhere": map[string]any{"type": "stdio", "command": "somewhere", "args": []string{"mcp"}},
				},
			}
			encoded, _ := json.Marshal(definition)
			args = append(args, "--mcp-config", string(encoded))
		}
	}

	request.Args = args
	return request, nil
}

func hasAnyArg(args []string, names ...string) bool {
	for _, arg := range args {
		for _, name := range names {
			if arg == name || strings.HasPrefix(arg, name+"=") {
				return true
			}
		}
	}
	return false
}

func configuredSomewhereMCP(configDir, cwd string) (bool, error) {
	home, _ := os.UserHomeDir()
	paths := make([]string, 0, 8)
	if configDir != "" {
		paths = append(paths, filepath.Join(configDir, ".claude.json"), filepath.Join(configDir, "settings.json"))
	} else if home != "" {
		paths = append(paths, filepath.Join(home, ".claude.json"), filepath.Join(home, ".claude", "settings.json"))
	}
	for current := filepath.Clean(cwd); current != "" && current != "."; current = filepath.Dir(current) {
		paths = append(paths, filepath.Join(current, ".mcp.json"))
		parent := filepath.Dir(current)
		if parent == current || (home != "" && current == filepath.Clean(home)) {
			break
		}
	}

	found := false
	for _, path := range paths {
		configured, conflict, err := somewhereMCPInFile(path)
		if err != nil {
			return false, err
		}
		if conflict {
			return false, fmt.Errorf("Claude already defines an MCP server named somewhere with a different target in %s; choose Inherit or resolve that configuration first", path)
		}
		found = found || configured
	}
	return found, nil
}

func somewhereMCPInFile(path string) (configured bool, conflict bool, err error) {
	encoded, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("inspect Claude MCP configuration %s: %w", path, err)
	}
	var document struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(encoded, &document); err != nil {
		return false, false, fmt.Errorf("inspect Claude MCP configuration %s: %w", path, err)
	}
	for name, definition := range document.MCPServers {
		canonical, err := isSomewhereMCPDefinition(definition)
		if err != nil {
			return false, false, fmt.Errorf("inspect Claude MCP configuration %s server %q: %w", path, name, err)
		}
		if strings.EqualFold(name, "somewhere") && !canonical {
			conflict = true
		}
		configured = configured || canonical
	}
	return configured, conflict, nil
}

func isSomewhereMCPDefinition(encoded json.RawMessage) (bool, error) {
	var definition struct {
		URL     string   `json:"url"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if err := json.Unmarshal(encoded, &definition); err != nil {
		return false, err
	}
	if definition.URL != "" {
		return strings.EqualFold(strings.TrimRight(strings.TrimSpace(definition.URL), "/"), somewhereMCPURL), nil
	}
	if !strings.EqualFold(filepath.Base(strings.TrimSpace(definition.Command)), "somewhere") {
		return false, nil
	}
	return len(definition.Args) > 0 && definition.Args[0] == "mcp", nil
}
