//go:build !windows

package tokenstore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func read(path string) (string, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		// Preserve the existing Unix CLI behavior: a missing or unreadable
		// optional local credential is treated as unavailable.
		return "", nil
	}
	return strings.TrimSpace(string(encoded)), nil
}

func readOrCreate(path string) (string, error) {
	if encoded, err := os.ReadFile(path); err == nil {
		value := strings.TrimSpace(string(encoded))
		if Valid(value) {
			if err := os.Chmod(path, 0o600); err != nil {
				return "", fmt.Errorf("chmod auth token: %w", err)
			}
			return value, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create token directory: %w", err)
	}
	value, err := generate()
	if err != nil {
		return "", fmt.Errorf("generate auth token: %w", err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		return "", fmt.Errorf("write auth token: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", fmt.Errorf("chmod auth token: %w", err)
	}
	return value, nil
}
