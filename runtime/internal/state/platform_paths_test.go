package state

import (
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// Every component that needs Sessions' own state must derive it from these
// helpers. A component that rebuilds the Unix layout by hand writes to a
// literal C:\Users\<user>\.local\... on Windows, where nothing reads it.
func TestUserStateAndConfigRootsAreThePlatformLocations(t *testing.T) {
	home := t.TempDir()
	stateRoot := UserStateRootFor(home)
	configRoot := UserConfigRootFor(home)

	if stateRoot == "" || configRoot == "" {
		t.Fatalf("state root %q / config root %q must not be empty", stateRoot, configRoot)
	}
	if stateRoot == configRoot {
		t.Fatalf("state and configuration must not share one directory: %q", stateRoot)
	}
	switch runtime.GOOS {
	case "windows":
		if !strings.HasSuffix(stateRoot, filepath.Join("Sessions", "state")) {
			t.Fatalf("windows state root = %q", stateRoot)
		}
		if !strings.HasSuffix(configRoot, filepath.Join("Sessions", "config")) {
			t.Fatalf("windows config root = %q", configRoot)
		}
		if strings.Contains(stateRoot, ".local") || strings.Contains(configRoot, ".config") {
			t.Fatalf("windows roots kept the Unix layout: %q / %q", stateRoot, configRoot)
		}
	default:
		if want := filepath.Join(home, ".local", "state", "sessions"); stateRoot != want {
			t.Fatalf("unix state root = %q, want %q", stateRoot, want)
		}
		// The Unix location is unchanged on purpose: moving it would strand
		// every existing install's state and backup key.
		if want := filepath.Join(home, ".config", "sessions"); configRoot != want {
			t.Fatalf("unix config root = %q, want %q", configRoot, want)
		}
	}
}

func TestConfigFromEnvUsesThePlatformRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SESSIONS_STATE_DIR", "")

	config, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if want := UserStateRootFor(homeFromConfig(t, config)); config.UserStateRoot != want {
		t.Fatalf("UserStateRoot = %q, want %q", config.UserStateRoot, want)
	}
	if want := filepath.Join(UserConfigRootFor(homeFromConfig(t, config)), "hooks.json"); config.GlobalHooksPath != want {
		t.Fatalf("GlobalHooksPath = %q, want %q", config.GlobalHooksPath, want)
	}
}

// homeFromConfig recovers the home ConfigFromEnv resolved so the assertions
// above do not depend on how the OS spells a temporary directory.
func homeFromConfig(t *testing.T, config Config) string {
	t.Helper()
	return config.DefaultCwd
}

func TestUserStateRootFromEnvMatchesUserStateRootFor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	got, err := UserStateRootFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if want := UserStateRootFor(home); got != want {
		t.Fatalf("UserStateRootFromEnv() = %q, want %q", got, want)
	}
	gotConfig, err := UserConfigRootFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if want := UserConfigRootFor(home); gotConfig != want {
		t.Fatalf("UserConfigRootFromEnv() = %q, want %q", gotConfig, want)
	}
}

// SESSIONS_STATE_DIR relocates the runner directory only. The user state root
// is what machine credentials, uploads, and backup configuration hang off, and
// a scratch daemon must not appear to move them.
func TestUserStateRootIgnoresSessionsStateDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SESSIONS_STATE_DIR", filepath.Join(t.TempDir(), "scratch"))

	config, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if config.UserStateRoot == config.StateRoot {
		t.Fatalf("SESSIONS_STATE_DIR did not isolate StateRoot: %q", config.StateRoot)
	}
	if want := UserStateRootFor(config.DefaultCwd); config.UserStateRoot != want {
		t.Fatalf("UserStateRoot = %q, want %q", config.UserStateRoot, want)
	}
}

func TestRunnerIDFromMetadataName(t *testing.T) {
	id := "2f577cd7-565b-4861-8ea2-c77c39a20e24"
	tests := []struct {
		name   string
		wantID string
		wantOK bool
	}{
		{name: id + ".json", wantID: id, wantOK: true},
		// The bug this guards: a session with a continuation sidecar produced a
		// phantom "<id>.continuation" runner id, which then failed to join
		// forever and buried the real lost-session signal.
		{name: id + ".continuation.json"},
		{name: id + ".manifest.json"},
		{name: id + ".events"},
		{name: id + ".log"},
		{name: id + ".sock"},
		{name: id + ".codexapp.jsonl"},
		{name: id + ".manifest.json.tmp"},
		{name: "." + id + ".json.tmp-12345"},
		{name: ".json"},
		{name: ""},
	}
	for _, test := range tests {
		gotID, gotOK := RunnerIDFromMetadataName(test.name)
		if gotOK != test.wantOK || gotID != test.wantID {
			t.Fatalf("RunnerIDFromMetadataName(%q) = (%q, %v), want (%q, %v)",
				test.name, gotID, gotOK, test.wantID, test.wantOK)
		}
	}
}

// Drift guard: adding a new "<id>.something.json" artifact to Paths without
// listing its suffix would silently reintroduce phantom runner ids on hosts
// that discover by metadata name.
func TestRunnerIDFromMetadataNameRejectsEveryJSONSidecarPathsCreates(t *testing.T) {
	id := "2f577cd7-565b-4861-8ea2-c77c39a20e24"
	paths := For(t.TempDir(), id)
	value := reflect.ValueOf(paths)
	for index := 0; index < value.NumField(); index++ {
		field := value.Type().Field(index)
		path, ok := value.Field(index).Interface().(string)
		if !ok || path == "" || field.Name == "Dir" || field.Name == "ID" {
			continue
		}
		name := filepath.Base(path)
		gotID, gotOK := RunnerIDFromMetadataName(name)
		if field.Name == "Meta" {
			if !gotOK || gotID != id {
				t.Fatalf("Paths.Meta %q was not recognized as the metadata file", name)
			}
			continue
		}
		if gotOK {
			t.Fatalf("Paths.%s (%q) was mistaken for runner id %q; add its suffix to runnerSidecarJSONSuffixes",
				field.Name, name, gotID)
		}
	}
}
