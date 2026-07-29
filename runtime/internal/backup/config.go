// Package backup sends opted-in conversation transcripts directly from the
// user's daemon to the user's own somewhere project.
package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	configVersion   = 1
	DefaultInterval = 15 * time.Minute
)

// Fingerprint is the incremental-upload identity of a local transcript.
type Fingerprint struct {
	Size        int64 `json:"size"`
	ModTimeNano int64 `json:"mtime_ns"`
}

// Config is deliberately token-free. TokenPath points at the somewhere CLI's
// config so token rotation does not require rewriting Sessions configuration.
type Config struct {
	Version          int                    `json:"version"`
	Enabled          bool                   `json:"enabled"`
	Encrypt          bool                   `json:"encrypt"`
	Project          string                 `json:"project"`
	TokenPath        string                 `json:"token_path"`
	Interval         string                 `json:"interval"`
	LastPushAt       string                 `json:"last_push_at,omitempty"`
	LastPushCount    int                    `json:"last_push_count"`
	LastPushSkipped  int                    `json:"last_push_skipped"`
	LastSessionCount int                    `json:"last_session_count"`
	Cache            map[string]Fingerprint `json:"cache,omitempty"`
}

// Status is the non-secret subset printed by the CLI and returned by the API.
type Status struct {
	Enabled          bool   `json:"enabled"`
	Encrypt          bool   `json:"encrypt"`
	KeyPath          string `json:"key_path,omitempty"`
	Project          string `json:"project,omitempty"`
	Interval         string `json:"interval,omitempty"`
	LastPushAt       string `json:"last_push_at,omitempty"`
	LastPushCount    int    `json:"last_push_count"`
	LastPushSkipped  int    `json:"last_push_skipped"`
	LastSessionCount int    `json:"last_session_count"`
}

func ConfigPath(home string) string {
	return filepath.Join(home, ".config", "sessions", "backup.json")
}

func SomewhereConfigPath(home string) string {
	return filepath.Join(home, ".somewhere", "config.json")
}

func (c Config) Status() Status {
	return Status{
		Enabled: c.Enabled, Encrypt: c.Encrypt, Project: c.Project, Interval: c.Interval,
		LastPushAt: c.LastPushAt, LastPushCount: c.LastPushCount,
		LastPushSkipped: c.LastPushSkipped, LastSessionCount: c.LastSessionCount,
	}
}

func (c Config) interval() (time.Duration, error) {
	value := c.Interval
	if value == "" {
		return DefaultInterval, nil
	}
	interval, err := time.ParseDuration(value)
	if err != nil || interval <= 0 {
		return 0, fmt.Errorf("invalid backup interval %q", value)
	}
	return interval, nil
}

func LoadConfig(path string) (Config, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var config Config
	if err := json.Unmarshal(encoded, &config); err != nil {
		return Config{}, fmt.Errorf("decode backup config: %w", err)
	}
	if config.Version != configVersion {
		return Config{}, fmt.Errorf("unsupported backup config version %d", config.Version)
	}
	if config.Cache == nil {
		config.Cache = make(map[string]Fingerprint)
	}
	if _, err := config.interval(); err != nil {
		return Config{}, err
	}
	if config.Enabled {
		if err := validateProject(config.Project); err != nil {
			return Config{}, err
		}
		if config.TokenPath == "" {
			return Config{}, errors.New("backup token path is empty")
		}
	}
	return config, nil
}

// Enable validates the existing somewhere credential but stores only its path.
func Enable(configPath, tokenPath, project string, interval time.Duration) (Config, error) {
	config, _, err := EnableWithEncryption(configPath, tokenPath, keyPathForConfig(configPath), project, interval, false)
	return config, err
}

// EnableWithEncryption preserves the existing plaintext flow while managing
// the optional local encryption key. Changing modes clears the upload cache so
// the next push rewrites every transcript in the selected format.
func EnableWithEncryption(configPath, tokenPath, keyPath, project string, interval time.Duration, encrypt bool) (Config, KeySetup, error) {
	if err := validateProject(project); err != nil {
		return Config{}, KeySetup{}, err
	}
	if interval <= 0 {
		interval = DefaultInterval
	}
	if _, err := ReadSomewhereToken(tokenPath); err != nil {
		return Config{}, KeySetup{}, err
	}
	config := Config{Version: configVersion, Cache: make(map[string]Fingerprint)}
	if existing, err := LoadConfig(configPath); err == nil {
		config = existing
	} else if !errors.Is(err, os.ErrNotExist) {
		return Config{}, KeySetup{}, err
	}
	previousEncrypt := config.Encrypt
	keySetup := KeySetup{}
	if encrypt {
		key, created, err := LoadOrCreateKey(keyPath)
		if err != nil {
			return Config{}, KeySetup{}, err
		}
		phrase, err := RecoveryPhrase(key)
		if err != nil {
			return Config{}, KeySetup{}, err
		}
		keySetup = KeySetup{RecoveryPhrase: phrase, Reused: !created}
	}
	config.Enabled = true
	config.Encrypt = encrypt
	config.Project = project
	config.TokenPath = tokenPath
	config.Interval = interval.String()
	if previousEncrypt != encrypt {
		config.Cache = make(map[string]Fingerprint)
	}
	if err := SaveConfig(configPath, config); err != nil {
		return Config{}, KeySetup{}, err
	}
	return config, keySetup, nil
}

func SaveConfig(path string, config Config) error {
	config.Version = configVersion
	if config.Cache == nil {
		config.Cache = make(map[string]Fingerprint)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create backup config directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".backup-*.tmp")
	if err != nil {
		return fmt.Errorf("create backup config temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		_ = temporary.Close()
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(config); err != nil {
		return fmt.Errorf("encode backup config: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace backup config: %w", err)
	}
	removeTemporary = false
	return os.Chmod(path, 0o600)
}

func validateProject(project string) error {
	if project == "" || project == "." || project == ".." {
		return errors.New("somewhere project is required")
	}
	for _, character := range project {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.' {
			continue
		}
		return fmt.Errorf("invalid somewhere project %q", project)
	}
	return nil
}

// ReadSomewhereToken accepts the somewhere CLI's JSON without binding Sessions
// to one config revision. It rejects ambiguity and never includes token bytes
// in errors.
func ReadSomewhereToken(path string) (string, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read somewhere config %s: %w", path, err)
	}
	var value any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return "", fmt.Errorf("decode somewhere config %s: %w", path, err)
	}
	tokens := make(map[string]struct{})
	collectSomewhereTokens(value, tokens)
	if len(tokens) == 0 {
		return "", fmt.Errorf("no smt_ token found in %s", path)
	}
	if len(tokens) > 1 {
		return "", fmt.Errorf("multiple smt_ tokens found in %s", path)
	}
	for token := range tokens {
		return token, nil
	}
	panic("unreachable")
}

func collectSomewhereTokens(value any, tokens map[string]struct{}) {
	switch typed := value.(type) {
	case string:
		if strings.HasPrefix(typed, "smt_") {
			tokens[typed] = struct{}{}
		}
	case []any:
		for _, item := range typed {
			collectSomewhereTokens(item, tokens)
		}
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			collectSomewhereTokens(typed[key], tokens)
		}
	}
}
