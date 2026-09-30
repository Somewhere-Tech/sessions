package state

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	DefaultHost = "127.0.0.1"
	DefaultPort = 8787
	DefaultCols = 300
	DefaultRows = 50
)

type Config struct {
	Host         string
	Port         int
	DefaultShell string
	DefaultCwd   string
	DefaultCols  int
	DefaultRows  int
	StateRoot    string
	// UserStateRoot is always the platform user state root
	// (~/.local/state/sessions on Unix, %LOCALAPPDATA%\Sessions\state on
	// Windows). Unlike the runner directory, idle and push state do not follow
	// SESSIONS_STATE_DIR.
	UserStateRoot      string
	RunnerStateDir     string
	TokenPath          string
	OpenPath           string
	MachineIDPath      string
	FleetAccountPath   string
	FleetKeyPath       string
	FleetURL           string
	FleetRelayEndpoint string
	SettingsPath       string
	LaunchAgentsDir    string
	GlobalHooksPath    string
	WebDir             string
	RunnerPath         string
	PprofAddress       string
}

func ConfigFromEnv() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("resolve home directory: %w", err)
	}

	host := getenv("SESSIONS_HOST", DefaultHost)
	port := DefaultPort
	if raw := os.Getenv("SESSIONS_PORT"); raw != "" {
		port, err = strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			return Config{}, fmt.Errorf("invalid SESSIONS_PORT %q", raw)
		}
	}

	stateRoot, userStateRoot, runnerDir, err := stateRootsFromEnv(home)
	if err != nil {
		return Config{}, err
	}

	webDir, err := resolveWebDir(os.Getenv("SESSIONS_WEB_DIR"))
	if err != nil {
		return Config{}, err
	}

	return Config{
		Host:               host,
		Port:               port,
		DefaultShell:       getenv("SHELL", defaultShell()),
		DefaultCwd:         getenv("HOME", home),
		DefaultCols:        DefaultCols,
		DefaultRows:        DefaultRows,
		StateRoot:          stateRoot,
		UserStateRoot:      userStateRoot,
		RunnerStateDir:     runnerDir,
		TokenPath:          filepath.Join(stateRoot, "token"),
		OpenPath:           filepath.Join(stateRoot, "open"),
		MachineIDPath:      filepath.Join(userStateRoot, "machine-id"),
		FleetAccountPath:   filepath.Join(stateRoot, "fleet-account.json"),
		FleetKeyPath:       filepath.Join(stateRoot, "fleet-machine-key.json"),
		FleetURL:           getenv("SESSIONS_FLEET_URL", "https://sessions-fleet.somewhere.site"),
		FleetRelayEndpoint: strings.TrimSpace(os.Getenv("SESSIONS_FLEET_RELAY_ENDPOINT")),
		SettingsPath:       filepath.Join(userStateRoot, "settings.json"),
		LaunchAgentsDir:    serviceDefinitionsDir(home),
		GlobalHooksPath:    filepath.Join(userConfigRoot(home), "hooks.json"),
		WebDir:             webDir,
		RunnerPath:         resolveRunnerPath(os.Getenv("SESSIONS_RUNNER")),
	}, nil
}

// LocalTokenPathFromEnv resolves the exact local token path used by sessionsd
// without loading unrelated daemon settings. The CLI uses this for loopback
// parity, including Windows LOCALAPPDATA and isolated SESSIONS_STATE_DIR state.
func LocalTokenPathFromEnv() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	stateRoot, _, _, err := stateRootsFromEnv(home)
	if err != nil {
		return "", err
	}
	return filepath.Join(stateRoot, "token"), nil
}

// UserStateRootFor is the platform user state root for one home directory:
// ~/.local/state/sessions on Unix and %LOCALAPPDATA%\Sessions\state on
// Windows. It is the single derivation every component must use instead of
// rebuilding the Unix layout by hand, so state stays in one place per host.
// SESSIONS_STATE_DIR deliberately does not move it; see Config.UserStateRoot.
func UserStateRootFor(home string) string {
	return defaultStateRoot(home)
}

// UserStateRootFromEnv is the sibling of LocalTokenPathFromEnv for callers that
// need the user state root without loading unrelated daemon settings.
func UserStateRootFromEnv() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return UserStateRootFor(home), nil
}

// UserConfigRootFor is the platform user configuration root for one home
// directory: ~/.config/sessions on Unix and %LOCALAPPDATA%\Sessions\config on
// Windows. Sessions' own configuration lives here; provider-owned config
// (Claude, Codex, somewhere) keeps its own vendor convention.
func UserConfigRootFor(home string) string {
	return userConfigRoot(home)
}

// UserConfigRootFromEnv resolves UserConfigRootFor against the current user.
func UserConfigRootFromEnv() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return UserConfigRootFor(home), nil
}

func stateRootsFromEnv(home string) (stateRoot, userStateRoot, runnerDir string, err error) {
	stateRoot = defaultStateRoot(home)
	userStateRoot = stateRoot
	runnerDir = filepath.Join(stateRoot, "runners")
	if configured := os.Getenv("SESSIONS_STATE_DIR"); configured != "" {
		runnerDir, err = filepath.Abs(configured)
		if err != nil {
			return "", "", "", fmt.Errorf("resolve SESSIONS_STATE_DIR: %w", err)
		}
		// SESSIONS_STATE_DIR is the TypeScript sessions.ts runner directory.
		// Keep token/open next to it when it is explicitly overridden so a
		// scratch daemon never consults the user's real state directory.
		if filepath.Base(runnerDir) == "runners" {
			stateRoot = filepath.Dir(runnerDir)
		} else {
			stateRoot = runnerDir
		}
	}
	return stateRoot, userStateRoot, runnerDir, nil
}

func (c Config) ListenAddress() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func resolveWebDir(explicit string) (string, error) {
	if explicit != "" {
		resolved, err := filepath.Abs(explicit)
		if err != nil {
			return "", fmt.Errorf("resolve SESSIONS_WEB_DIR: %w", err)
		}
		return resolved, nil
	}

	// Match http.ts: prefer the checkout's frontend/dist when present,
	// otherwise fall back to a web directory bundled beside the binary.
	candidates := make([]string, 0, 4)
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(cwd, "frontend", "dist"),
			filepath.Join(cwd, "..", "frontend", "dist"),
		)
	}
	if executable, err := os.Executable(); err == nil {
		dir := filepath.Dir(executable)
		candidates = append(candidates,
			filepath.Join(dir, "..", "frontend", "dist"),
			filepath.Join(dir, "web"),
		)
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return filepath.Abs(candidate)
		}
	}
	if executable, err := os.Executable(); err == nil {
		return filepath.Abs(filepath.Join(filepath.Dir(executable), "web"))
	}
	return filepath.Abs(filepath.Join("web"))
}

func resolveRunnerPath(explicit string) string {
	executable, _ := os.Executable()
	return resolveRunnerPathFrom(explicit, executable, runtime.GOOS, runtime.GOARCH)
}

func resolveRunnerPathFrom(explicit, executable, goos, goarch string) string {
	if explicit != "" {
		if resolved, err := filepath.Abs(explicit); err == nil && isExecutableFile(resolved) {
			return resolved
		}
		return ""
	}
	if executable != "" {
		dir := filepath.Dir(executable)
		for _, name := range runnerBinaryNames(goos, goarch) {
			candidate := filepath.Join(dir, name)
			if isExecutableFile(candidate) {
				return candidate
			}
		}
	}
	return ""
}
