package api

import (
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestNativeResumeHonorsExplicitFullPermissions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	daemon := newTestDaemon(t)
	daemon.config.SettingsPath = filepath.Join(home, "settings.json")
	if err := state.SaveSettings(daemon.config.SettingsPath, state.Settings{Onboarding: &state.OnboardingSettings{
		Version: state.OnboardingCurrentVersion, RemoteControlConsent: state.RemoteControlConsentEnabled,
		DelegatedAccessConsent: state.DelegatedAccessConsentInherited,
	}}); err != nil {
		t.Fatal(err)
	}
	manager := sessionruntime.NewManager(daemon.config, daemon.launcher, sessionruntime.ManagerOptions{DisableWatchers: true})
	t.Cleanup(manager.Close)
	daemon.handler = New(daemon.config, manager)
	writeAdoptableClaudeConversation(t, home, daemon.root, doubleOpenConversation)
	body := strings.NewReader(`{"target":"` + doubleOpenConversation + `","permissions":"full","runtimeMode":"terminal","remoteControl":true}`)
	response := serve(t, daemon.handler, http.MethodPost, "/api/recovery/adopt", body, "127.0.0.1:1", nil)
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var result recovery.AdoptResult
	decodeBody(t, response, &result)
	resumed, ok := manager.Get(result.LaneID)
	if !ok {
		t.Fatal("no resumed runtime")
	}
	info := resumed.Info()
	if info.Permissions != "full" || !slices.Contains(info.Args, "--dangerously-skip-permissions") || !slices.Contains(info.Args, "--remote-control") || !slices.Contains(info.Args, doubleOpenConversation) {
		t.Fatalf("resume did not honor explicit options: %#v", info)
	}
}
