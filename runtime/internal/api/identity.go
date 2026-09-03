package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

const maximumMachineName = 80

type machineIdentity struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func loadOrCreateMachineIdentity(config state.Config) (machineIdentity, error) {
	return loadOrCreateMachineIdentityWith(config, state.ComputerName, os.Hostname)
}

func loadOrCreateMachineIdentityWith(
	config state.Config,
	computerName func() string,
	dnsHostname func() (string, error),
) (machineIdentity, error) {
	root := config.UserStateRoot
	if root == "" {
		root = config.StateRoot
	}
	if root == "" || !filepath.IsAbs(root) {
		return machineIdentity{}, errors.New("machine identity requires an absolute user state directory")
	}
	path := config.MachineIDPath
	if path == "" {
		path = filepath.Join(root, "machine-id")
	}
	if !filepath.IsAbs(path) {
		return machineIdentity{}, errors.New("machine identity path must be absolute")
	}

	name := truncateMachineName(computerName())
	if name == "" {
		name = "Sessions machine"
	}
	identity, legacy, err := readMachineIdentity(path)
	if errors.Is(err, os.ErrNotExist) {
		return createMachineIdentity(path, name)
	}
	if err != nil {
		return machineIdentity{}, err
	}

	rawName, _ := dnsHostname()
	rawName = truncateMachineName(rawName)
	storedName := truncateMachineName(identity.Name)
	if legacy || storedName == "" {
		storedName = rawName
	}
	// The UUID is the durable machine identity. The name is display metadata:
	// upgrade the old DNS-derived default once, but never replace the ID or a
	// name that no longer matches that legacy default.
	if storedName == "" || (rawName != "" && storedName == rawName) {
		storedName = name
	}
	if legacy || storedName != identity.Name {
		identity.Name = storedName
		if err := writeMachineIdentity(path, identity); err != nil {
			return machineIdentity{}, err
		}
	}
	return identity, nil
}

func readMachineIdentity(path string) (machineIdentity, bool, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return machineIdentity{}, false, err
	}
	legacyID := strings.TrimSpace(string(encoded))
	identity := machineIdentity{}
	legacy := validMachineID(legacyID)
	if legacy {
		identity.ID = legacyID
	} else if err := json.Unmarshal(encoded, &identity); err != nil || !validMachineID(identity.ID) {
		return machineIdentity{}, false, fmt.Errorf("machine identity file is invalid: %s", path)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return machineIdentity{}, false, fmt.Errorf("protect machine identity: %w", err)
	}
	return identity, legacy, nil
}

func createMachineIdentity(path, name string) (machineIdentity, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return machineIdentity{}, fmt.Errorf("create machine identity directory: %w", err)
	}
	id, err := randomDeviceUUID()
	if err != nil {
		return machineIdentity{}, fmt.Errorf("generate machine identity: %w", err)
	}
	identity := machineIdentity{ID: id, Name: truncateMachineName(name)}
	if identity.Name == "" {
		identity.Name = "Sessions machine"
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return machineIdentity{}, fmt.Errorf("encode machine identity: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".machine-id-*")
	if err != nil {
		return machineIdentity{}, fmt.Errorf("create temporary machine identity: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return machineIdentity{}, fmt.Errorf("protect temporary machine identity: %w", err)
	}
	if _, err := temporary.Write(append(encoded, '\n')); err != nil {
		temporary.Close()
		return machineIdentity{}, fmt.Errorf("write machine identity: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return machineIdentity{}, fmt.Errorf("close machine identity: %w", err)
	}
	// A hard link publishes the fully-written temporary file without replacing
	// an identity created by a racing daemon startup.
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			if existing, _, readErr := readMachineIdentity(path); readErr == nil {
				return existing, nil
			}
		}
		return machineIdentity{}, fmt.Errorf("publish machine identity: %w", err)
	}
	return identity, nil
}

func writeMachineIdentity(path string, identity machineIdentity) error {
	encoded, err := json.Marshal(identity)
	if err != nil {
		return fmt.Errorf("encode machine identity: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".machine-id-*")
	if err != nil {
		return fmt.Errorf("create temporary machine identity: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("protect temporary machine identity: %w", err)
	}
	if _, err := temporary.Write(append(encoded, '\n')); err != nil {
		temporary.Close()
		return fmt.Errorf("write machine identity: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close machine identity: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace machine identity: %w", err)
	}
	return nil
}

func validMachineID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return value[14] == '4' && strings.ContainsRune("89ab", rune(value[19]))
}

func truncateMachineName(value string) string {
	return truncateRunes(strings.TrimSpace(value), maximumMachineName)
}

func truncateRunes(value string, maximum int) string {
	runes := []rune(value)
	if len(runes) <= maximum {
		return value
	}
	return string(runes[:maximum])
}
