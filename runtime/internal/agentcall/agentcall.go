// Package agentcall runs a bounded, tool-disabled request through a user's
// already-authenticated Codex or Claude CLI. It deliberately contains no model
// client and strips provider API-key environment variables so Sessions never
// turns an opt-in convenience into an accidental pay-per-token API path.
package agentcall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	ProviderCodex  = "codex"
	ProviderClaude = "claude"
)

var requiredCodexFeatures = []string{
	"shell_tool", "unified_exec", "code_mode_host", "apps", "plugins",
	"browser_use", "browser_use_external", "browser_use_full_cdp_access", "in_app_browser",
	"computer_use", "image_generation", "multi_agent", "goals", "hooks", "remote_plugin",
	"workspace_dependencies", "skill_mcp_dependency_install", "tool_suggest", "auth_elicitation",
	"tool_call_mcp_elicitation",
}

var validatedCodexExecutables sync.Map

// Run executes one isolated request. The selected CLI chooses its own default
// model; Sessions only asks for low reasoning effort and disables tools.
func Run(ctx context.Context, provider, purpose, prompt string) (string, error) {
	executable, err := Executable(provider)
	if err != nil {
		return "", err
	}
	if err := validateIsolationSupport(ctx, provider, executable); err != nil {
		return "", err
	}
	workingDirectory, err := os.MkdirTemp("", "sessions-agent-call-*")
	if err != nil {
		return "", fmt.Errorf("create isolated %s workspace: %w", purpose, err)
	}
	defer os.RemoveAll(workingDirectory)

	command := exec.CommandContext(ctx, executable, Arguments(provider)...)
	command.Dir = workingDirectory
	command.Stdin = strings.NewReader(prompt)
	command.Env = Environment()
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("%s timed out", purpose)
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("%s %s call failed: %s", provider, purpose, detail)
	}
	return stdout.String(), nil
}

// Arguments are intentionally model-free. User CLI defaults decide the model.
func Arguments(provider string) []string {
	if provider == ProviderClaude {
		return []string{
			"-p", "--effort", "low", "--tools", "", "--strict-mcp-config",
			"--safe-mode", "--no-chrome", "--disable-slash-commands", "--setting-sources", "",
			"--no-session-persistence", "--output-format", "text",
		}
	}
	// Read-only sandboxing still permits reads through Codex's shell and other
	// tools. Disable every tool-bearing feature explicitly so the planner can
	// only transform the prompt supplied on stdin.
	args := []string{
		"--ask-for-approval", "never",
		"-c", `model_reasoning_effort="low"`,
		"-c", `web_search="disabled"`,
	}
	for _, feature := range requiredCodexFeatures {
		args = append(args, "--disable", feature)
	}
	return append(args, "exec", "--ephemeral", "--ignore-user-config", "--ignore-rules", "--sandbox", "read-only", "--skip-git-repo-check", "--color", "never", "-")
}

func validateIsolationSupport(ctx context.Context, provider, executable string) error {
	if provider != ProviderCodex {
		return nil
	}
	if _, ok := validatedCodexExecutables.Load(executable); ok {
		return nil
	}
	validationCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(validationCtx, executable, "features", "list")
	command.Dir = os.TempDir()
	command.Env = Environment()
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("verify Codex isolation support: %w; update Codex or select Claude in Sessions Settings", err)
	}
	missing := missingCodexFeatures(string(output))
	if len(missing) > 0 {
		return fmt.Errorf(
			"Codex CLI is too old for isolated smart features (missing %s); update Codex or select Claude in Sessions Settings",
			strings.Join(missing, ", "),
		)
	}
	validatedCodexExecutables.Store(executable, struct{}{})
	return nil
}

func missingCodexFeatures(output string) []string {
	available := make([]string, 0)
	for _, line := range strings.Split(output, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			available = append(available, fields[0])
		}
	}
	missing := make([]string, 0)
	for _, required := range requiredCodexFeatures {
		if !slices.Contains(available, required) {
			missing = append(missing, required)
		}
	}
	return missing
}

func Executable(provider string) (string, error) {
	name := provider
	if provider == ProviderClaude {
		name = "claude"
	}
	if provider != ProviderCodex && provider != ProviderClaude {
		return "", fmt.Errorf("unknown AI provider %q; choose codex or claude", provider)
	}
	if resolved, err := exec.LookPath(name); err == nil {
		return resolved, nil
	}
	home, _ := os.UserHomeDir()
	for _, candidate := range []string{
		filepath.Join(home, ".local", "bin", name),
		filepath.Join("/opt/homebrew/bin", name),
		filepath.Join("/usr/local/bin", name),
	} {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s CLI is not installed or not on PATH; install it and sign in before using %s", name, name)
}

func Environment() []string {
	environment := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "ANTHROPIC_API_KEY=") || strings.HasPrefix(entry, "OPENAI_API_KEY=") {
			continue
		}
		environment = append(environment, entry)
	}
	home, _ := os.UserHomeDir()
	extra := strings.Join([]string{filepath.Join(home, ".local", "bin"), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin"}, ":")
	found := false
	for index, entry := range environment {
		if strings.HasPrefix(entry, "PATH=") {
			environment[index] = "PATH=" + extra + ":" + strings.TrimPrefix(entry, "PATH=")
			found = true
			break
		}
	}
	if !found {
		environment = append(environment, "PATH="+extra)
	}
	return environment
}
