package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	NotifyDone    = "done"
	NotifyWaiting = "waiting"
	NotifyLost    = "lost"

	RecapProviderOff    = "off"
	RecapProviderCodex  = "codex"
	RecapProviderClaude = "claude"

	AIProviderCodex  = "codex"
	AIProviderClaude = "claude"

	ClaudeChoiceInherit = "inherit"
	ClaudeChoiceOn      = "on"
	ClaudeChoiceOff     = "off"

	ClaudePermissionManual      = "manual"
	ClaudePermissionAcceptEdits = "acceptEdits"
	ClaudePermissionAuto        = "auto"
	ClaudePermissionPlan        = "plan"
	ClaudePermissionDontAsk     = "dontAsk"
	ClaudePermissionBypass      = "bypassPermissions"

	ClaudeSomewhereEnsure = "ensure"
)

var settingsMu sync.Mutex

type NotifySettings struct {
	Done    bool `json:"done"`
	Waiting bool `json:"waiting"`
	Lost    bool `json:"lost"`
}

// RecapSettings is deliberately opt-in. Provider "off" means Sessions never
// launches a model for daily synthesis. The selected CLI chooses its own
// default model; Sessions only requests the provider's lowest reasoning effort.
type RecapSettings struct {
	Provider string `json:"provider"`
}

// AISettings selects the pre-authenticated CLI used for explicit smart
// features such as natural-language search planning. It defaults to Codex,
// but no call happens until the user submits an AI action.
type AISettings struct {
	Provider string `json:"provider"`
}

// ClaudeSettings contains launch defaults owned by Sessions. "inherit" means
// no corresponding CLI override is supplied, leaving Claude's own effective
// configuration authoritative. Credentials and provider settings files are
// never rewritten.
type ClaudeSettings struct {
	RemoteControl           string `json:"remoteControl"`
	PermissionMode          string `json:"permissionMode"`
	Model                   string `json:"model"`
	Effort                  string `json:"effort"`
	Chrome                  string `json:"chrome"`
	SomewhereMCP            string `json:"somewhereMcp"`
	RemoteControlNamePrefix string `json:"remoteControlNamePrefix"`
}

func DefaultClaudeSettings() ClaudeSettings {
	return ClaudeSettings{
		RemoteControl:  ClaudeChoiceInherit,
		PermissionMode: ClaudeChoiceInherit,
		Effort:         ClaudeChoiceInherit,
		Chrome:         ClaudeChoiceInherit,
		SomewhereMCP:   ClaudeChoiceInherit,
	}
}

func NormalizeClaudeSettings(settings ClaudeSettings) (ClaudeSettings, error) {
	defaults := DefaultClaudeSettings()
	settings.RemoteControl = defaultString(settings.RemoteControl, defaults.RemoteControl)
	settings.PermissionMode = defaultString(settings.PermissionMode, defaults.PermissionMode)
	settings.Effort = defaultString(settings.Effort, defaults.Effort)
	settings.Chrome = defaultString(settings.Chrome, defaults.Chrome)
	settings.SomewhereMCP = defaultString(settings.SomewhereMCP, defaults.SomewhereMCP)
	settings.Model = strings.TrimSpace(settings.Model)
	settings.RemoteControlNamePrefix = strings.TrimSpace(settings.RemoteControlNamePrefix)

	if !oneOf(settings.RemoteControl, ClaudeChoiceInherit, ClaudeChoiceOn, ClaudeChoiceOff) {
		return ClaudeSettings{}, fmt.Errorf("unknown Claude Remote Control default %q", settings.RemoteControl)
	}
	if !oneOf(settings.PermissionMode,
		ClaudeChoiceInherit,
		ClaudePermissionManual,
		ClaudePermissionAcceptEdits,
		ClaudePermissionAuto,
		ClaudePermissionPlan,
		ClaudePermissionDontAsk,
		ClaudePermissionBypass,
	) {
		return ClaudeSettings{}, fmt.Errorf("unknown Claude permission mode %q", settings.PermissionMode)
	}
	if !oneOf(settings.Effort, ClaudeChoiceInherit, "low", "medium", "high", "xhigh", "max") {
		return ClaudeSettings{}, fmt.Errorf("unknown Claude effort %q", settings.Effort)
	}
	if !oneOf(settings.Chrome, ClaudeChoiceInherit, ClaudeChoiceOn, ClaudeChoiceOff) {
		return ClaudeSettings{}, fmt.Errorf("unknown Claude Chrome default %q", settings.Chrome)
	}
	if !oneOf(settings.SomewhereMCP, ClaudeChoiceInherit, ClaudeSomewhereEnsure) {
		return ClaudeSettings{}, fmt.Errorf("unknown Somewhere MCP default %q", settings.SomewhereMCP)
	}
	if err := validateClaudeText("model", settings.Model, 128); err != nil {
		return ClaudeSettings{}, err
	}
	if err := validateClaudeText("Remote Control name prefix", settings.RemoteControlNamePrefix, 64); err != nil {
		return ClaudeSettings{}, err
	}
	return settings, nil
}

// ResolveClaudeSettings overlays the non-empty per-session choices on the
// persisted defaults. An explicit per-session "inherit" therefore suppresses
// a Sessions default for that one launch.
func ResolveClaudeSettings(defaults ClaudeSettings, overrides *ClaudeSessionOptions) (ClaudeSettings, error) {
	resolved, err := NormalizeClaudeSettings(defaults)
	if err != nil {
		return ClaudeSettings{}, err
	}
	if overrides == nil {
		return resolved, nil
	}
	if value := strings.TrimSpace(overrides.RemoteControl); value != "" {
		resolved.RemoteControl = value
	}
	if value := strings.TrimSpace(overrides.PermissionMode); value != "" {
		resolved.PermissionMode = value
	}
	if value := strings.TrimSpace(overrides.Model); value != "" {
		resolved.Model = value
	}
	if value := strings.TrimSpace(overrides.Effort); value != "" {
		resolved.Effort = value
	}
	if value := strings.TrimSpace(overrides.Chrome); value != "" {
		resolved.Chrome = value
	}
	if value := strings.TrimSpace(overrides.SomewhereMCP); value != "" {
		resolved.SomewhereMCP = value
	}
	if value := strings.TrimSpace(overrides.RemoteControlNamePrefix); value != "" {
		resolved.RemoteControlNamePrefix = value
	}
	return NormalizeClaudeSettings(resolved)
}

func defaultString(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func validateClaudeText(label, value string, maximum int) error {
	if len(value) > maximum {
		return fmt.Errorf("Claude %s is too long (maximum %d characters)", label, maximum)
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return fmt.Errorf("Claude %s contains control characters", label)
		}
	}
	return nil
}

func DefaultAISettings() AISettings {
	return AISettings{Provider: AIProviderCodex}
}

func NormalizeAISettings(settings AISettings) (AISettings, error) {
	settings.Provider = strings.ToLower(strings.TrimSpace(settings.Provider))
	if settings.Provider == "" {
		settings.Provider = AIProviderCodex
	}
	if settings.Provider != AIProviderCodex && settings.Provider != AIProviderClaude {
		return AISettings{}, fmt.Errorf("unknown AI provider %q; choose codex or claude", settings.Provider)
	}
	return settings, nil
}

func DefaultRecapSettings() RecapSettings {
	return RecapSettings{Provider: RecapProviderOff}
}

func NormalizeRecapSettings(settings RecapSettings) (RecapSettings, error) {
	settings.Provider = strings.ToLower(strings.TrimSpace(settings.Provider))
	if settings.Provider == "" {
		settings.Provider = RecapProviderOff
	}
	if settings.Provider != RecapProviderOff && settings.Provider != RecapProviderCodex && settings.Provider != RecapProviderClaude {
		return RecapSettings{}, fmt.Errorf("unknown recap provider %q; choose off, codex, or claude", settings.Provider)
	}
	return settings, nil
}

func DefaultNotifySettings() NotifySettings {
	return NotifySettings{Done: true, Waiting: true, Lost: true}
}

func (n NotifySettings) Enabled(kind string) bool {
	switch kind {
	case NotifyDone:
		return n.Done
	case NotifyWaiting:
		return n.Waiting
	case NotifyLost:
		return n.Lost
	default:
		return false
	}
}

func (n *NotifySettings) Set(kind string, enabled bool) error {
	switch kind {
	case "":
		n.Done = enabled
		n.Waiting = enabled
		n.Lost = enabled
	case NotifyDone:
		n.Done = enabled
	case NotifyWaiting:
		n.Waiting = enabled
	case NotifyLost:
		n.Lost = enabled
	default:
		return fmt.Errorf("unknown notification kind %q; choose done, waiting, or lost", kind)
	}
	return nil
}

// Settings contains daemon choices which persist independently of runner
// state. Additive fields keep this file easy to extend without changing its
// location or format.
type Settings struct {
	LAN    bool            `json:"lan"`
	Notify *NotifySettings `json:"notify,omitempty"`
	Recap  *RecapSettings  `json:"recap,omitempty"`
	AI     *AISettings     `json:"ai,omitempty"`
	Claude *ClaudeSettings `json:"claude,omitempty"`
}

func (s Settings) EffectiveNotify() NotifySettings {
	if s.Notify == nil {
		return DefaultNotifySettings()
	}
	return *s.Notify
}

func (s Settings) EffectiveRecap() RecapSettings {
	if s.Recap == nil {
		return DefaultRecapSettings()
	}
	return *s.Recap
}

func (s Settings) EffectiveAI() AISettings {
	if s.AI == nil {
		return DefaultAISettings()
	}
	return *s.AI
}

func (s Settings) EffectiveClaude() ClaudeSettings {
	if s.Claude == nil {
		return DefaultClaudeSettings()
	}
	return *s.Claude
}

func LoadSettings(path string) (Settings, error) {
	encoded, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Settings{}, nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("read settings: %w", err)
	}
	var settings Settings
	if err := json.Unmarshal(encoded, &settings); err != nil {
		return Settings{}, fmt.Errorf("decode settings: %w", err)
	}
	return settings, nil
}

func SaveSettings(path string, settings Settings) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create settings directory: %w", err)
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".settings-*")
	if err != nil {
		return fmt.Errorf("create temporary settings: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("chmod temporary settings: %w", err)
	}
	if _, err := temporary.Write(append(encoded, '\n')); err != nil {
		temporary.Close()
		return fmt.Errorf("write settings: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close settings: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace settings: %w", err)
	}
	return nil
}

// UpdateSettings serializes read-modify-write callers which share the daemon's
// settings file so independent settings sections are not accidentally erased.
func UpdateSettings(path string, update func(*Settings) error) error {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	settings, err := LoadSettings(path)
	if err != nil {
		return err
	}
	if err := update(&settings); err != nil {
		return err
	}
	return SaveSettings(path, settings)
}
