package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSettingsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "settings.json")
	settings, err := LoadSettings(path)
	if err != nil || settings.LAN {
		t.Fatalf("missing settings = %#v, %v", settings, err)
	}
	if notify := settings.EffectiveNotify(); !notify.Done || !notify.Waiting || !notify.Lost {
		t.Fatalf("missing notify defaults = %#v", notify)
	}
	if err := SaveSettings(path, Settings{LAN: true}); err != nil {
		t.Fatal(err)
	}
	settings, err = LoadSettings(path)
	if err != nil || !settings.LAN {
		t.Fatalf("loaded settings = %#v, %v", settings, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("settings mode = %04o, want 0600", info.Mode().Perm())
	}
	if err := SaveSettings(path, Settings{}); err != nil {
		t.Fatal(err)
	}
	settings, err = LoadSettings(path)
	if err != nil || settings.LAN {
		t.Fatalf("disabled settings = %#v, %v", settings, err)
	}
	if err := UpdateSettings(path, func(settings *Settings) error {
		notify := settings.EffectiveNotify()
		if err := notify.Set(NotifyWaiting, false); err != nil {
			return err
		}
		settings.Notify = &notify
		settings.LAN = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	settings, err = LoadSettings(path)
	if err != nil || !settings.LAN {
		t.Fatalf("updated settings = %#v, %v", settings, err)
	}
	notify := settings.EffectiveNotify()
	if !notify.Done || notify.Waiting || !notify.Lost {
		t.Fatalf("updated notify settings = %#v", notify)
	}
	if err := notify.Set("unknown", true); err == nil {
		t.Fatal("unknown notification kind was accepted")
	}
}

func TestNormalizeRecapSettings(t *testing.T) {
	settings, err := NormalizeRecapSettings(RecapSettings{Provider: " CODEX "})
	if err != nil {
		t.Fatal(err)
	}
	if settings.Provider != RecapProviderCodex {
		t.Fatalf("settings = %#v", settings)
	}
	if _, err := NormalizeRecapSettings(RecapSettings{Provider: "hosted"}); err == nil {
		t.Fatal("unknown provider was accepted")
	}
	if got := (Settings{}).EffectiveRecap(); got.Provider != RecapProviderOff {
		t.Fatalf("default recap = %#v", got)
	}
}

func TestNormalizeAISettings(t *testing.T) {
	settings, err := NormalizeAISettings(AISettings{Provider: " CLAUDE "})
	if err != nil {
		t.Fatal(err)
	}
	if settings.Provider != AIProviderClaude {
		t.Fatalf("settings = %#v", settings)
	}
	if _, err := NormalizeAISettings(AISettings{Provider: "hosted"}); err == nil {
		t.Fatal("unknown provider was accepted")
	}
	if got := (Settings{}).EffectiveAI(); got.Provider != AIProviderCodex {
		t.Fatalf("default AI = %#v", got)
	}
}

func TestNormalizeAndResolveClaudeSettings(t *testing.T) {
	defaults := DefaultClaudeSettings()
	if defaults.RemoteControl != ClaudeChoiceOn || defaults.PermissionMode != ClaudeChoiceInherit || defaults.SomewhereMCP != ClaudeChoiceInherit {
		t.Fatalf("default Claude settings = %#v", defaults)
	}
	normalized, err := NormalizeClaudeSettings(ClaudeSettings{
		RemoteControl: ClaudeChoiceOn, PermissionMode: ClaudePermissionManual,
		Model: " opus ", Effort: "high", Chrome: ClaudeChoiceOff,
		SomewhereMCP: ClaudeSomewhereEnsure, RemoteControlNamePrefix: " sessions ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Model != "opus" || normalized.RemoteControlNamePrefix != "sessions" {
		t.Fatalf("normalized Claude settings = %#v", normalized)
	}
	resolved, err := ResolveClaudeSettings(normalized, &ClaudeSessionOptions{
		RemoteControl: ClaudeChoiceInherit, PermissionMode: ClaudePermissionPlan, Model: "sonnet",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.RemoteControl != ClaudeChoiceInherit || resolved.PermissionMode != ClaudePermissionPlan || resolved.Model != "sonnet" || resolved.SomewhereMCP != ClaudeSomewhereEnsure {
		t.Fatalf("resolved Claude settings = %#v", resolved)
	}
	for _, invalid := range []ClaudeSettings{
		{RemoteControl: "always"},
		{PermissionMode: "danger"},
		{Effort: "infinite"},
		{Chrome: "sometimes"},
		{SomewhereMCP: "proxy"},
		{Model: "bad\nmodel"},
	} {
		if _, err := NormalizeClaudeSettings(invalid); err == nil {
			t.Fatalf("invalid Claude settings accepted: %#v", invalid)
		}
	}
}

func TestLegacyClaudeInheritMigratesWithoutOverridingNewExplicitChoice(t *testing.T) {
	legacy := Settings{Claude: &ClaudeSettings{RemoteControl: ClaudeChoiceInherit}}
	if got := legacy.EffectiveClaude().RemoteControl; got != ClaudeChoiceOn {
		t.Fatalf("legacy Remote Control = %q, want %q", got, ClaudeChoiceOn)
	}
	versioned := Settings{
		Claude:                  &ClaudeSettings{RemoteControl: ClaudeChoiceInherit},
		ClaudeExperienceVersion: 1,
	}
	if got := versioned.EffectiveClaude().RemoteControl; got != ClaudeChoiceInherit {
		t.Fatalf("explicit versioned Remote Control = %q, want %q", got, ClaudeChoiceInherit)
	}
}
