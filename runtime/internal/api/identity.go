package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

const maximumMachineName = 80

type machineIdentity struct {
	ID   string
	Name string
}

func loadOrCreateMachineIdentity(config state.Config) (machineIdentity, error) {
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

	id, err := readMachineID(path)
	if errors.Is(err, os.ErrNotExist) {
		id, err = createMachineID(path)
	}
	if err != nil {
		return machineIdentity{}, err
	}
	name, _ := os.Hostname()
	name = truncateMachineName(name)
	if name == "" {
		name = "Sessions machine"
	}
	return machineIdentity{ID: id, Name: name}, nil
}

func readMachineID(path string) (string, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(encoded))
	if !validMachineID(id) {
		return "", fmt.Errorf("machine identity file is invalid: %s", path)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", fmt.Errorf("protect machine identity: %w", err)
	}
	return id, nil
}

func createMachineID(path string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create machine identity directory: %w", err)
	}
	id, err := randomDeviceUUID()
	if err != nil {
		return "", fmt.Errorf("generate machine identity: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".machine-id-*")
	if err != nil {
		return "", fmt.Errorf("create temporary machine identity: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return "", fmt.Errorf("protect temporary machine identity: %w", err)
	}
	if _, err := temporary.WriteString(id + "\n"); err != nil {
		temporary.Close()
		return "", fmt.Errorf("write machine identity: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close machine identity: %w", err)
	}
	// A hard link publishes the fully-written temporary file without replacing
	// an identity created by a racing daemon startup.
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			if existing, readErr := readMachineID(path); readErr == nil {
				return existing, nil
			}
		}
		return "", fmt.Errorf("publish machine identity: %w", err)
	}
	return id, nil
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
