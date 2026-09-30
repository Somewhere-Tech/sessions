package api

import (
	"encoding/json"
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
	if first.Name == "" || first.Name != second.Name {
		t.Fatalf("machine identity names are not stable: first=%#v second=%#v", first, second)
	}
	info, err := os.Stat(config.MachineIDPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("machine identity mode = %o, want 600", info.Mode().Perm())
	}
}

func TestMachineIdentityCreationUsesComputerName(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "machine-id")
	identity, err := loadOrCreateMachineIdentityWith(
		state.Config{StateRoot: root, UserStateRoot: root, MachineIDPath: path},
		func() string { return "Uzair's MacBook Pro" },
		func() (string, error) { return "MacBook-Pro-10.local", nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !validMachineID(identity.ID) || identity.Name != "Uzair's MacBook Pro" {
		t.Fatalf("created identity = %#v, want a durable ID and the computer name", identity)
	}
	persisted, legacy, err := readMachineIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if legacy || persisted != identity {
		t.Fatalf("persisted identity = %#v legacy=%v, want %#v", persisted, legacy, identity)
	}
}

func TestMachineIdentityUpgradesLegacyDNSNameWithoutChangingID(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "machine-id")
	wantID := "11111111-2222-4333-8444-555555abcdef"
	stored := machineIdentity{ID: wantID, Name: "MacBook-Pro-10.local"}
	encoded, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	config := state.Config{StateRoot: root, UserStateRoot: root, MachineIDPath: path}
	identity, err := loadOrCreateMachineIdentityWith(
		config,
		func() string { return "Uzair's MacBook Pro" },
		func() (string, error) { return "MacBook-Pro-10.local", nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ID != wantID || identity.Name != "Uzair's MacBook Pro" {
		t.Fatalf("upgraded identity = %#v, want unchanged ID and friendly name", identity)
	}

	persisted, legacy, err := readMachineIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if legacy || persisted != identity {
		t.Fatalf("persisted identity = %#v legacy=%v, want %#v", persisted, legacy, identity)
	}

	// Once the old DNS-derived default has been replaced, a later lookup does
	// not overwrite the stored display name.
	again, err := loadOrCreateMachineIdentityWith(
		config,
		func() string { return "A different friendly name" },
		func() (string, error) { return "MacBook-Pro-10.local", nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if again != identity {
		t.Fatalf("second load changed upgraded identity: got %#v want %#v", again, identity)
	}
}

func TestMachineIdentityMigratesLegacyUUIDFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "machine-id")
	wantID := "11111111-2222-4333-8444-555555abcdef"
	if err := os.WriteFile(path, []byte(wantID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := loadOrCreateMachineIdentityWith(
		state.Config{StateRoot: root, UserStateRoot: root, MachineIDPath: path},
		func() string { return "Uzair's MacBook Pro" },
		func() (string, error) { return "MacBook-Pro-10.local", nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ID != wantID || identity.Name != "Uzair's MacBook Pro" {
		t.Fatalf("migrated identity = %#v, want unchanged ID and friendly name", identity)
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
