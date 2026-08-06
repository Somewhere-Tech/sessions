// Package agentcall runs a bounded, tool-disabled request through a user's
// already-authenticated Codex or Claude CLI. It deliberately contains no model
// client, and it builds the child environment from an allowlist so Sessions
// never turns an opt-in convenience into an accidental pay-per-token API path
// and never redirects the call to a host the user did not choose.
//
// The allowlist is the whole promise. A denylist of "known billing variables"
// loses to the next one a vendor ships: ANTHROPIC_AUTH_TOKEN is a supported
// Claude Code credential, CLAUDE_CODE_USE_BEDROCK and CLAUDE_CODE_USE_VERTEX
// reroute the same call onto a cloud account with its own bill, and
// ANTHROPIC_BASE_URL or OPENAI_BASE_URL send the prompt — which contains the
// user's session titles and text — to a third party. None of those names
// appear below, because nothing appears below that is not needed to find and
// run the CLI the user already signed in to.
package agentcall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	ProviderCodex  = "codex"
	ProviderClaude = "claude"
)

// defaultCallTimeout bounds one provider call when the caller supplied no
// deadline of its own. Callers that do bound the call keep their own budget.
const defaultCallTimeout = 5 * time.Minute

// waitDelay bounds the wait for the CLI's output pipes once the call is
// cancelled or its deadline passes. Both provider CLIs fork helper processes
// that inherit the pipe, so without it Wait blocks on the pipe copy long after
// the direct child is gone and the timeout below is never reached. It is a
// variable only so tests can shorten it.
var waitDelay = 5 * time.Second

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
	ctx, cancel := boundedContext(ctx)
	defer cancel()
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

	return runIsolated(ctx, provider, purpose, executable, Arguments(provider), workingDirectory, prompt)
}

// boundedContext defends the daemon against a caller that never bounds its own
// call. Both callers serialize on a single slot — daily recap holds a generate
// mutex and smart search a one-deep busy channel — so an unbounded hung CLI
// would take the feature down until the daemon restarts.
func boundedContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, defaultCallTimeout)
}

func runIsolated(ctx context.Context, provider, purpose, executable string, arguments []string, workingDirectory, prompt string) (string, error) {
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Dir = workingDirectory
	command.Stdin = strings.NewReader(prompt)
	command.Env = Environment()
	command.WaitDelay = waitDelay
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	started := time.Now()
	err := command.Run()
	if err == nil {
		return stdout.String(), nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf(
			"%s timed out after %s and nothing was generated; check that the %s CLI is installed and signed in, then try again",
			purpose, time.Since(started).Round(time.Second), provider,
		)
	}
	if ctx.Err() != nil {
		return "", fmt.Errorf("%s was cancelled before the %s CLI answered; nothing was generated", purpose, provider)
	}
	// The CLI itself finished cleanly and only a helper process it forked kept
	// the output pipe open past WaitDelay. The captured answer is complete.
	if errors.Is(err, exec.ErrWaitDelay) {
		return stdout.String(), nil
	}
	detail := strings.TrimSpace(stderr.String())
	if detail == "" {
		detail = err.Error()
	}
	return "", fmt.Errorf("%s %s call failed: %s", provider, purpose, detail)
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
	command.WaitDelay = waitDelay
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
	// exec.LookPath honours PATH as the daemon inherited it, which under a GUI
	// launcher or a service manager often omits the user's package-manager
	// directories. Probe them directly, with the platform's executable
	// extensions applied.
	for _, directory := range providerSearchDirectories() {
		for _, candidate := range executableCandidates(filepath.Join(directory, name)) {
			if isExecutableFile(candidate) {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("%s CLI is not installed or not on PATH; install it and sign in before using %s", name, name)
}

// passedThroughEnvironment is the complete set of variable names an agent call
// inherits. Everything else is dropped. Names are compared case-insensitively
// because Windows environment blocks are case-insensitive; on Unix the extra
// tolerance only ever admits an oddly-cased spelling of a name already listed
// here, which the child would ignore anyway.
//
// Each entry earns its place by being needed to locate the CLI, let it read
// the credentials the user already stored on disk, or let it talk to the
// network at all. Nothing here selects a model, an account, or an endpoint.
var passedThroughEnvironment = map[string]struct{}{
	// Locate the user's home and per-user configuration.
	"HOME": {}, "USERPROFILE": {}, "HOMEDRIVE": {}, "HOMEPATH": {},
	"XDG_CONFIG_HOME": {}, "XDG_CACHE_HOME": {}, "XDG_DATA_HOME": {}, "XDG_STATE_HOME": {},
	"APPDATA": {}, "LOCALAPPDATA": {}, "PROGRAMDATA": {},

	// Where each CLI keeps the session the user already signed in with.
	"CLAUDE_CONFIG_DIR": {}, "CODEX_HOME": {},

	// Process basics. PATH is rebuilt below rather than inherited verbatim.
	"PATH": {}, "PATHEXT": {}, "SHELL": {}, "COMSPEC": {}, "TERM": {},
	"TMPDIR": {}, "TMP": {}, "TEMP": {},
	"USER": {}, "USERNAME": {}, "LOGNAME": {},
	"LANG": {}, "LC_ALL": {}, "LC_CTYPE": {},

	// Windows needs these for a Node-based CLI to start at all.
	"SYSTEMROOT": {}, "SYSTEMDRIVE": {}, "WINDIR": {}, "PROGRAMFILES": {}, "PROGRAMFILES(X86)": {},
	"PROCESSOR_ARCHITECTURE": {}, "NUMBER_OF_PROCESSORS": {}, "OS": {},

	// Corporate egress. A proxy or a private CA bundle decides how the call
	// reaches the provider the CLI already chose; neither picks the provider,
	// and without them the CLI cannot reach the network on a managed machine.
	"HTTP_PROXY": {}, "HTTPS_PROXY": {}, "NO_PROXY": {}, "ALL_PROXY": {},
	"http_proxy": {}, "https_proxy": {}, "no_proxy": {}, "all_proxy": {},
	"NODE_EXTRA_CA_CERTS": {}, "SSL_CERT_FILE": {}, "SSL_CERT_DIR": {},
}

func Environment() []string {
	environment := make([]string, 0, len(passedThroughEnvironment)+1)
	inheritedPATH := ""
	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, allowed := passedThroughEnvironment[strings.ToUpper(name)]; !allowed {
			continue
		}
		if strings.EqualFold(name, "PATH") {
			inheritedPATH = value
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment, "PATH="+childPATH(inheritedPATH))
}

// childPATH puts the directories package managers actually install these CLIs
// into ahead of whatever the daemon inherited. It joins with the platform's
// list separator: on Windows that is ';', and using ':' would fuse the prefix
// and the first inherited entry into one unusable path split at a drive colon.
func childPATH(inherited string) string {
	directories := append([]string(nil), providerSearchDirectories()...)
	if inherited != "" {
		directories = append(directories, inherited)
	}
	return strings.Join(directories, string(os.PathListSeparator))
}

// providerSearchDirectories are the platform's usual install locations for the
// Codex and Claude CLIs. They are also the fallback probe list in Executable.
func providerSearchDirectories() []string {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "windows" {
		directories := make([]string, 0, 6)
		if appData := os.Getenv("APPDATA"); appData != "" {
			directories = append(directories, filepath.Join(appData, "npm"))
		}
		if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
			directories = append(directories, filepath.Join(localAppData, "Programs"))
		}
		if programFiles := os.Getenv("ProgramFiles"); programFiles != "" {
			directories = append(directories, filepath.Join(programFiles, "nodejs"))
		}
		if systemRoot := os.Getenv("SystemRoot"); systemRoot != "" {
			directories = append(directories, filepath.Join(systemRoot, "System32"), systemRoot)
		}
		if home != "" {
			directories = append(directories, filepath.Join(home, ".local", "bin"))
		}
		return directories
	}
	directories := make([]string, 0, 5)
	if home != "" {
		directories = append(directories, filepath.Join(home, ".local", "bin"))
	}
	return append(directories, "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin")
}

// executableCandidates mirrors the PATHEXT-aware resolver in internal/state:
// on Windows a bare name is not runnable, and the CLIs ship as .cmd or .exe.
func executableCandidates(path string) []string {
	if runtime.GOOS != "windows" || filepath.Ext(path) != "" {
		return []string{path}
	}
	extensions := filepath.SplitList(os.Getenv("PATHEXT"))
	if len(extensions) == 0 {
		extensions = []string{".COM", ".EXE", ".BAT", ".CMD"}
	}
	candidates := []string{path}
	for _, extension := range extensions {
		if extension = strings.TrimSpace(extension); extension != "" {
			candidates = append(candidates, path+strings.ToLower(extension), path+strings.ToUpper(extension))
		}
	}
	return candidates
}

// isExecutableFile answers the platform's own question. Go synthesizes 0666 for
// every regular file on Windows, so an execute-bit test there is always false
// and would silently disable the whole fallback.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0o111 != 0
}
