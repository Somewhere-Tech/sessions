package state

import "strings"

func configureProfileEnvironment(env map[string]string, tool SessionTool, profile, configDir string) {
	stripProfileAuthOverrides(env, tool)
	key := "CODEX_HOME"
	if tool == ToolClaude {
		key = "CLAUDE_CONFIG_DIR"
	}
	env[key] = configDir
	env["RUNNER_PROFILE"] = profile
	env["RUNNER_CONFIG_DIR"] = configDir
}

// A named subscription must not silently use an ambient API key, OAuth token,
// gateway, or another account's app-server socket instead of its provider home.
func stripProfileAuthOverrides(env map[string]string, tool SessionTool) {
	for key := range env {
		normalized := strings.ToUpper(key)
		if tool == ToolCodex && (strings.HasPrefix(normalized, "OPENAI_") || normalized == "CODEX_API_KEY" || normalized == "SESSIONS_CODEX_APP_SERVER_SOCKET") {
			delete(env, key)
		}
		if tool == ToolClaude && (strings.HasPrefix(normalized, "ANTHROPIC_") || normalized == "CLAUDE_CODE_OAUTH_TOKEN" || strings.HasPrefix(normalized, "CLAUDE_CODE_USE_")) {
			delete(env, key)
		}
	}
}
