package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestMachineIdentityPersistsAcrossDaemonStarts(t *testing.T) {
	root := t.TempDir()
	config := state.Config{
		StateRoot:      root,
		UserStateRoot:  root,
		MachineIDPath:  filepath.Join(root, "machine-id"),
		RunnerStateDir: filepath.Join(root, "runners"),
	}
	first, err := loadOrCreateMachineIdentity(config)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadOrCreateMachineIdentity(config)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || first.ID != second.ID || !validMachineID(first.ID) {
		t.Fatalf("machine identities are not stable: first=%#v second=%#v", first, second)
	}
	info, err := os.Stat(config.MachineIDPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("machine identity mode = %o, want 600", info.Mode().Perm())
	}
}

func TestMachineIdentityRejectsCorruptPersistedValue(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "machine-id")
	if err := os.WriteFile(path, []byte("not-a-machine-id\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadOrCreateMachineIdentity(state.Config{
		StateRoot: root, UserStateRoot: root, MachineIDPath: path,
	})
	if err == nil {
		t.Fatal("corrupt machine identity was silently replaced")
	}
}
