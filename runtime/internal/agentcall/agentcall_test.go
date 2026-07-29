package agentcall

import (
	"slices"
	"strings"
	"testing"
)

func TestCodexArgumentsDisableToolBearingFeatures(t *testing.T) {
	arguments := Arguments(ProviderCodex)
	joined := strings.Join(arguments, " ")
	for _, feature := range []string{
		"shell_tool", "unified_exec", "code_mode_host", "apps", "plugins",
		"browser_use", "browser_use_external", "browser_use_full_cdp_access", "in_app_browser",
		"computer_use", "image_generation", "multi_agent", "goals", "hooks", "remote_plugin",
		"workspace_dependencies", "skill_mcp_dependency_install", "tool_suggest", "auth_elicitation",
		"tool_call_mcp_elicitation",
	} {
		if !hasPair(arguments, "--disable", feature) {
			t.Errorf("Codex arguments do not disable %s: %s", feature, joined)
		}
	}
	if !hasPair(arguments, "-c", `web_search="disabled"`) {
		t.Errorf("Codex arguments do not disable web search: %s", joined)
	}
	for _, required := range []string{"--ephemeral", "--ignore-user-config", "--ignore-rules", "read-only"} {
		if !slices.Contains(arguments, required) {
			t.Errorf("Codex arguments missing %q: %s", required, joined)
		}
	}
	if strings.Contains(joined, "--model") {
		t.Fatalf("Codex arguments hardcode a model: %s", joined)
	}
}

func TestClaudeArgumentsDisableToolsAndPersistence(t *testing.T) {
	arguments := Arguments(ProviderClaude)
	if !hasPair(arguments, "--tools", "") || !slices.Contains(arguments, "--strict-mcp-config") ||
		!slices.Contains(arguments, "--safe-mode") || !slices.Contains(arguments, "--no-chrome") ||
		!slices.Contains(arguments, "--disable-slash-commands") || !hasPair(arguments, "--setting-sources", "") ||
		!slices.Contains(arguments, "--no-session-persistence") {
		t.Fatalf("Claude arguments are not isolated: %#v", arguments)
	}
}

func TestMissingCodexFeatures(t *testing.T) {
	var output strings.Builder
	for _, feature := range requiredCodexFeatures {
		if feature != "hooks" {
			output.WriteString(feature + " stable true\n")
		}
	}
	missing := missingCodexFeatures(output.String())
	if len(missing) != 1 || missing[0] != "hooks" {
		t.Fatalf("missing=%#v, want hooks", missing)
	}
}

func hasPair(values []string, first, second string) bool {
	for index := 0; index+1 < len(values); index++ {
		if values[index] == first && values[index+1] == second {
			return true
		}
	}
	return false
}
