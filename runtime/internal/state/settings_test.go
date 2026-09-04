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
	if remote := settings.EffectiveRemote(); !remote.Auto {
		t.Fatalf("missing remote default = %#v", remote)
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

func TestRemoteAutoSettingRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := SaveSettings(path, Settings{Remote: &RemoteSettings{Auto: false}}); err != nil {
		t.Fatal(err)
	}
	settings, err := LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Remote == nil || settings.EffectiveRemote().Auto {
		t.Fatalf("remote setting = %#v", settings.Remote)
	}
}

func TestLoadSettingsIgnoresLegacyRecapAndUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	encoded := []byte(`{"lan":true,"recap":{"provider":"codex","schedule":"daily"},"future":{"enabled":true}}`)
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings() rejected legacy or unknown keys: %v", err)
	}
	if !settings.LAN {
		t.Fatalf("LoadSettings() lost known settings: %#v", settings)
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
	if defaults.RemoteControl != ClaudeChoiceOff || defaults.PermissionMode != ClaudeChoiceInherit || defaults.SomewhereMCP != ClaudeChoiceInherit {
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

func TestRemoteControlRequiresCompletedOnboardingConsent(t *testing.T) {
	for name, settings := range map[string]Settings{
		"fresh install": {},
		"legacy inherit": {
			Claude: &ClaudeSettings{RemoteControl: ClaudeChoiceInherit},
		},
		"legacy on": {
			Claude: &ClaudeSettings{RemoteControl: ClaudeChoiceOn},
		},
		"reset onboarding": {
			Claude:     &ClaudeSettings{RemoteControl: ClaudeChoiceOn},
			Onboarding: &OnboardingSettings{Version: 0, RemoteControlConsent: RemoteControlConsentEnabled},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if state := settings.EffectiveOnboarding(); state.Complete || state.RemoteControl != RemoteControlConsentPending {
				t.Fatalf("onboarding = %#v", state)
			}
			if got := settings.EffectiveClaude().RemoteControl; got != ClaudeChoiceOff {
				t.Fatalf("Remote Control = %q, want %q", got, ClaudeChoiceOff)
			}
		})
	}

	enabled := Settings{
		Claude: &ClaudeSettings{RemoteControl: ClaudeChoiceOn},
		Onboarding: &OnboardingSettings{
			Version: OnboardingCurrentVersion, RemoteControlConsent: RemoteControlConsentEnabled,
		},
	}
	if state := enabled.EffectiveOnboarding(); !state.Complete || state.RemoteControl != RemoteControlConsentEnabled {
		t.Fatalf("enabled onboarding = %#v", state)
	}
	if got := enabled.EffectiveClaude().RemoteControl; got != ClaudeChoiceOn {
		t.Fatalf("enabled Remote Control = %q, want %q", got, ClaudeChoiceOn)
	}

	localOnly := Settings{
		Claude: &ClaudeSettings{RemoteControl: ClaudeChoiceOff},
		Onboarding: &OnboardingSettings{
			Version: OnboardingCurrentVersion, RemoteControlConsent: RemoteControlConsentLocalOnly,
		},
	}
	if state := localOnly.EffectiveOnboarding(); !state.Complete || state.RemoteControl != RemoteControlConsentLocalOnly {
		t.Fatalf("local-only onboarding = %#v", state)
	}
	if got := localOnly.EffectiveClaude().RemoteControl; got != ClaudeChoiceOff {
		t.Fatalf("local-only Remote Control = %q, want %q", got, ClaudeChoiceOff)
	}
}

func TestDelegatedAccessRequiresCurrentExplicitOnboarding(t *testing.T) {
	if got := (Settings{}).EffectiveDelegation().Access; got != DelegatedAccessConsentAutonomous {
		t.Fatalf("fresh delegation = %q, want inherit", got)
	}
	legacy := Settings{
		Delegation: &DelegationSettings{Access: DelegatedAccessConsentAutonomous},
		Onboarding: &OnboardingSettings{
			Version: OnboardingCurrentVersion - 1, RemoteControlConsent: RemoteControlConsentLocalOnly,
			DelegatedAccessConsent: DelegatedAccessConsentAutonomous,
		},
	}
	if got := legacy.EffectiveDelegation().Access; got != DelegatedAccessConsentAutonomous {
		t.Fatalf("legacy delegation = %q, want inherit", got)
	}
	autonomous := Settings{
		Delegation: &DelegationSettings{Access: DelegatedAccessConsentAutonomous},
		Onboarding: &OnboardingSettings{
			Version: OnboardingCurrentVersion, RemoteControlConsent: RemoteControlConsentLocalOnly,
			DelegatedAccessConsent: DelegatedAccessConsentAutonomous,
		},
	}
	state := autonomous.EffectiveOnboarding()
	if !state.Complete || state.DelegatedAccess != DelegatedAccessConsentAutonomous {
		t.Fatalf("autonomous onboarding = %#v", state)
	}
	if got := autonomous.EffectiveDelegation().Access; got != DelegatedAccessConsentAutonomous {
		t.Fatalf("autonomous delegation = %q", got)
	}
}
