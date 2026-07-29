package state

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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
	// UserStateRoot is always ~/.local/state/sessions. Unlike the runner
	// directory, idle and push state do not follow SESSIONS_STATE_DIR.
	UserStateRoot   string
	RunnerStateDir  string
	TokenPath       string
	OpenPath        string
	MachineIDPath   string
	SettingsPath    string
	LaunchAgentsDir string
	GlobalHooksPath string
	WebDir          string
	RunnerPath      string
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
		Host:            host,
		Port:            port,
		DefaultShell:    getenv("SHELL", defaultShell()),
		DefaultCwd:      getenv("HOME", home),
		DefaultCols:     DefaultCols,
		DefaultRows:     DefaultRows,
		StateRoot:       stateRoot,
		UserStateRoot:   userStateRoot,
		RunnerStateDir:  runnerDir,
		TokenPath:       filepath.Join(stateRoot, "token"),
		OpenPath:        filepath.Join(stateRoot, "open"),
		MachineIDPath:   filepath.Join(userStateRoot, "machine-id"),
		SettingsPath:    filepath.Join(userStateRoot, "settings.json"),
		LaunchAgentsDir: serviceDefinitionsDir(home),
		GlobalHooksPath: filepath.Join(home, ".config", "sessions", "hooks.json"),
		WebDir:          webDir,
		RunnerPath:      resolveRunnerPath(os.Getenv("SESSIONS_RUNNER")),
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
