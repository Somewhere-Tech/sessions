package backup

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	keySize             = chacha20poly1305.KeySize
	recoveryGroupCount  = 8
	recoveryGroupBytes  = keySize / recoveryGroupCount
	recoveryGroupLength = 7
)

var (
	recoveryEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)
	// ErrWrongKeyOrCorruptedFile deliberately hides the underlying AEAD error,
	// which is not actionable and can expose implementation details.
	ErrWrongKeyOrCorruptedFile = errors.New("wrong key or corrupted file")
)

// KeySetup describes the local key used while enabling encrypted backups.
type KeySetup struct {
	RecoveryPhrase string
	Reused         bool
}

func KeyPath(home string) string {
	return filepath.Join(home, ".config", "sessions", "backup.key")
}

func keyPathForConfig(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "backup.key")
}

// LoadOrCreateKey returns the existing backup key or creates one from secure
// random bytes. It never replaces an existing key.
func LoadOrCreateKey(path string) ([]byte, bool, error) {
	key, err := ReadKey(path)
	if err == nil {
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, false, fmt.Errorf("secure backup key %s: %w", path, err)
		}
		return key, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}

	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, false, fmt.Errorf("create backup key directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, false, fmt.Errorf("secure backup key directory: %w", err)
	}
	key = make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, false, fmt.Errorf("generate backup key: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		key, readErr := ReadKey(path)
		if readErr != nil {
			return nil, false, readErr
		}
		return key, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("create backup key %s: %w", path, err)
	}
	created := true
	defer func() {
		if created {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(key); err != nil {
		_ = file.Close()
		return nil, false, fmt.Errorf("write backup key %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return nil, false, fmt.Errorf("sync backup key %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return nil, false, fmt.Errorf("close backup key %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, false, fmt.Errorf("secure backup key %s: %w", path, err)
	}
	created = false
	return key, true, nil
}

func ReadKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read backup key %s: %w", path, err)
	}
	if len(key) != keySize {
		return nil, fmt.Errorf("invalid backup key %s: expected %d bytes", path, keySize)
	}
	return key, nil
}

// RecoveryPhrase formats a key as eight independently decodable base32
// groups. Each seven-character group represents exactly four key bytes.
func RecoveryPhrase(key []byte) (string, error) {
	if len(key) != keySize {
		return "", fmt.Errorf("invalid backup key: expected %d bytes", keySize)
	}
	groups := make([]string, 0, recoveryGroupCount)
	for offset := 0; offset < len(key); offset += recoveryGroupBytes {
		groups = append(groups, recoveryEncoding.EncodeToString(key[offset:offset+recoveryGroupBytes]))
	}
	return strings.Join(groups, " "), nil
}

// KeyFromRecoveryPhrase accepts the printed space-separated form, as well as
// lowercase input or hyphens in place of spaces.
func KeyFromRecoveryPhrase(phrase string) ([]byte, error) {
	groups := strings.Fields(strings.ReplaceAll(strings.TrimSpace(phrase), "-", " "))
	if len(groups) != recoveryGroupCount {
		return nil, fmt.Errorf("invalid recovery phrase: expected %d base32 groups", recoveryGroupCount)
	}
	key := make([]byte, 0, keySize)
	for index, group := range groups {
		if len(group) != recoveryGroupLength {
			return nil, fmt.Errorf("invalid recovery phrase: group %d must be %d characters", index+1, recoveryGroupLength)
		}
		decoded, err := recoveryEncoding.DecodeString(strings.ToUpper(group))
		if err != nil || len(decoded) != recoveryGroupBytes {
			return nil, fmt.Errorf("invalid recovery phrase: group %d is not valid base32", index+1)
		}
		key = append(key, decoded...)
	}
	return key, nil
}

// Encrypt seals plaintext with XChaCha20-Poly1305. The returned payload is the
// fresh 24-byte nonce followed by the authenticated ciphertext.
func Encrypt(key, plaintext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("invalid backup key: expected %d bytes", keySize)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate encryption nonce: %w", err)
	}
	return aead.Seal(nonce, nonce, plaintext, nil), nil
}

func Decrypt(key, payload []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("invalid backup key: expected %d bytes", keySize)
	}
	if len(payload) < aead.NonceSize()+aead.Overhead() {
		return nil, ErrWrongKeyOrCorruptedFile
	}
	plaintext, err := aead.Open(nil, payload[:aead.NonceSize()], payload[aead.NonceSize():], nil)
	if err != nil {
		return nil, ErrWrongKeyOrCorruptedFile
	}
	return plaintext, nil
}

func DecryptFile(inputPath, outputPath string, key []byte) error {
	payload, err := os.ReadFile(inputPath)
	if err != nil {
		return fmt.Errorf("read encrypted backup %s: %w", inputPath, err)
	}
	plaintext, err := Decrypt(key, payload)
	if err != nil {
		return err
	}
	if err := writePrivateFile(outputPath, plaintext); err != nil {
		return fmt.Errorf("write decrypted backup %s: %w", outputPath, err)
	}
	return nil
}

func writePrivateFile(path string, contents []byte) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".sessions-decrypt-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		_ = temporary.Close()
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	removeTemporary = false
	return os.Chmod(path, 0o600)
}
