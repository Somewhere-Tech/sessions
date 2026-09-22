package state

import "testing"

func TestProfileEnvironmentCannotUseAnotherAccount(t *testing.T) {
	for _, tool := range []SessionTool{ToolClaude, ToolCodex} {
		env := map[string]string{"PATH": "safe", "HTTPS_PROXY": "safe", "OPENAI_API_KEY": "wrong", "ANTHROPIC_AUTH_TOKEN": "wrong", "CLAUDE_CODE_OAUTH_TOKEN": "wrong", "SESSIONS_CODEX_APP_SERVER_SOCKET": "wrong"}
		configureProfileEnvironment(env, tool, "work", "/private/work")
		keys := []string{"OPENAI_API_KEY", "SESSIONS_CODEX_APP_SERVER_SOCKET"}
		if tool == ToolClaude {
			keys = []string{"ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}
		}
		for _, key := range keys {
			if _, ok := env[key]; ok {
				t.Fatalf("%s leaked %s", tool, key)
			}
		}
		if env["PATH"] != "safe" || env["HTTPS_PROXY"] != "safe" || env["RUNNER_PROFILE"] != "work" {
			t.Fatal("lost unrelated environment or profile")
		}
	}
}
