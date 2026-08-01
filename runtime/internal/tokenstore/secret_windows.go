//go:build windows

package tokenstore

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const maxDeviceCredentialSize = 64 * 1024

type protectedDeviceCredential struct {
	Version   int    `json:"version"`
	Protected string `json:"protected"`
}

func readSecret(path string) (string, error) {
	encoded, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read protected device credential: %w", err)
	}
	if len(encoded) > maxDeviceCredentialSize {
		return "", errors.New("protected device credential is unexpectedly large")
	}
	if err := protectPath(path); err != nil {
		return "", err
	}
	var envelope protectedDeviceCredential
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		legacy := strings.TrimSpace(string(encoded))
		if err := validateDeviceCredential(legacy); err != nil {
			return "", fmt.Errorf("parse protected device credential: %w", err)
		}
		if err := writeSecret(path, legacy); err != nil {
			return "", fmt.Errorf("protect legacy device credential: %w", err)
		}
		return legacy, nil
	}
	if envelope.Version != 1 {
		return "", fmt.Errorf("unsupported protected device credential version %d", envelope.Version)
	}
	protected, err := base64.StdEncoding.DecodeString(envelope.Protected)
	if err != nil || len(protected) == 0 {
		return "", errors.New("protected device credential contains invalid ciphertext")
	}
	plaintext, err := dpapiUnprotect(deviceCredentialEntropyPath(path), protected)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(plaintext))
	if err := validateDeviceCredential(value); err != nil {
		return "", err
	}
	return value, nil
}

func writeSecret(path, value string) error {
	value = strings.TrimSpace(value)
	if err := validateDeviceCredential(value); err != nil {
		return err
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create device credential directory: %w", err)
	}
	if err := applyOwnerACL(parent, true); err != nil {
		return err
	}
	protected, err := dpapiProtect(deviceCredentialEntropyPath(path), []byte(value))
	if err != nil {
		return err
	}
	envelope, err := json.Marshal(protectedDeviceCredential{
		Version: 1, Protected: base64.StdEncoding.EncodeToString(protected),
	})
	if err != nil {
		return err
	}
	envelope = append(envelope, '\n')
	temporary, err := os.CreateTemp(parent, ".device-credential-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	keep := true
	defer func() {
		if keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(envelope); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := applyOwnerACL(temporaryPath, false); err != nil {
		return err
	}
	if err := replaceFile(temporaryPath, path); err != nil {
		return err
	}
	keep = false
	verified, err := readSecret(path)
	if err != nil {
		return fmt.Errorf("verify protected device credential: %w", err)
	}
	if verified != value {
		return errors.New("protected device credential did not read back exactly")
	}
	return nil
}

func validateDeviceCredential(value string) error {
	if value == "" || len(value) > 512 || strings.ContainsAny(value, "\r\n") {
		return errors.New("invalid device credential")
	}
	return nil
}

func deviceCredentialEntropyPath(path string) string {
	return filepath.Join(filepath.Dir(path), "device:"+filepath.Base(path))
}
