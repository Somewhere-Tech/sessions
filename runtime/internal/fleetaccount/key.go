package fleetaccount

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
)

type machineKeyFile struct {
	Version int    `json:"version"`
	Public  string `json:"public_key"`
	Private string `json:"private_key"`
}

type keyStore struct {
	mu   sync.Mutex
	path string
}

func (s *keyStore) loadOrCreate() (ed25519.PrivateKey, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	private, public, err := s.load()
	if !errors.Is(err, os.ErrNotExist) {
		return private, public, err
	}
	return s.create()
}

func (s *keyStore) load() (ed25519.PrivateKey, string, error) {
	encoded, err := os.ReadFile(s.path)
	if err != nil {
		return nil, "", err
	}
	var file machineKeyFile
	if err := json.Unmarshal(encoded, &file); err != nil || file.Version != 1 {
		return nil, "", errors.New("fleet machine key file is invalid")
	}
	private, err := base64.RawURLEncoding.DecodeString(file.Private)
	if err != nil || len(private) != ed25519.PrivateKeySize {
		return nil, "", errors.New("fleet machine private key is invalid")
	}
	public := private[32:]
	if base64.RawURLEncoding.EncodeToString(public) != file.Public {
		return nil, "", errors.New("fleet machine public key does not match private key")
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		return nil, "", fmt.Errorf("protect fleet machine key: %w", err)
	}
	return ed25519.PrivateKey(private), file.Public, nil
}

func (s *keyStore) create() (ed25519.PrivateKey, string, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("generate fleet machine key: %w", err)
	}
	publicText := base64.RawURLEncoding.EncodeToString(public)
	file := machineKeyFile{
		Version: 1, Public: publicText,
		Private: base64.RawURLEncoding.EncodeToString(private),
	}
	if err := writePrivateJSON(s.path, file); err != nil {
		return nil, "", err
	}
	return private, publicText, nil
}
