package api

import (
	"path/filepath"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestUploadsDirComesFromConfiguredState(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	userRoot := state.UserStateRootFor(home)

	tests := []struct {
		name   string
		config state.Config
		want   string
	}{
		{
			name:   "default install uses the platform user state root",
			config: state.Config{StateRoot: userRoot, UserStateRoot: userRoot},
			want:   filepath.Join(userRoot, "uploads"),
		},
		{
			name:   "an isolated SESSIONS_STATE_DIR daemon keeps uploads to itself",
			config: state.Config{StateRoot: filepath.Join(home, "scratch"), UserStateRoot: userRoot},
			want:   filepath.Join(home, "scratch", "uploads"),
		},
		{
			name:   "an unconfigured server still derives the platform location",
			config: state.Config{},
			want:   filepath.Join(userRoot, "uploads"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := &Server{config: test.config}
			if got := server.uploadsDir(home); got != test.want {
				t.Fatalf("uploadsDir() = %q, want %q", got, test.want)
			}
		})
	}
}

// backupHome used to reverse-derive the home directory by asserting the literal
// ".../.local/state/sessions" spelling. A Windows user state root ends in
// "state", so it always failed, server.backups was never built, and every
// backup route answered 503 with no way for the user to tell why.
func TestBackupHomeAcceptsEveryPlatformStateRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	tests := []struct {
		name    string
		root    string
		wantOK  bool
		comment string
	}{
		{name: "platform user state root", root: state.UserStateRootFor(home), wantOK: true},
		{name: "unix layout", root: filepath.Join(home, ".local", "state", "sessions"), wantOK: true},
		{name: "windows layout", root: filepath.Join(home, "AppData", "Local", "Sessions", "state"), wantOK: true},
		{name: "empty", root: "", wantOK: false},
		{name: "outside the home", root: filepath.Join(t.TempDir(), "elsewhere", "state"), wantOK: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := backupHome(test.root)
			if ok != test.wantOK {
				t.Fatalf("backupHome(%q) ok = %v, want %v", test.root, ok, test.wantOK)
			}
			if ok && got != filepath.Clean(home) {
				t.Fatalf("backupHome(%q) = %q, want %q", test.root, got, home)
			}
		})
	}
}
