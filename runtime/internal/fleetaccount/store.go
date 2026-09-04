package fleetaccount

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

const stateVersion = 1

type store struct {
	mu   sync.Mutex
	path string
}

func (s *store) load() (persistedState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *store) loadLocked() (persistedState, error) {
	encoded, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return persistedState{Version: stateVersion}, nil
	}
	if err != nil {
		return persistedState{}, fmt.Errorf("read fleet account: %w", err)
	}
	var state persistedState
	if err := json.Unmarshal(encoded, &state); err != nil || state.Version != stateVersion {
		return persistedState{}, errors.New("fleet account file is invalid")
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		return persistedState{}, fmt.Errorf("protect fleet account: %w", err)
	}
	return state, nil
}

func (s *store) update(change func(*persistedState)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadLocked()
	if err != nil {
		return err
	}
	change(&state)
	state.Version = stateVersion
	return writePrivateJSON(s.path, state)
}

func (s *store) clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove fleet account: %w", err)
	}
	return nil
}

func (s *store) applyRotation(header http.Header) error {
	access := header.Get("X-New-Access-Token")
	refresh := header.Get("X-New-Refresh-Token")
	if access == "" || refresh == "" {
		return nil
	}
	return s.update(func(state *persistedState) {
		state.Tokens.AccessToken = access
		state.Tokens.RefreshToken = refresh
	})
}

func writePrivateJSON(path string, value any) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create private state directory: %w", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode private state: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".fleet-state-*.tmp")
	if err != nil {
		return fmt.Errorf("stage private state: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := writePrivateTemporary(temporary, append(encoded, '\n')); err != nil {
		return err
	}
	if err := replacePrivateFile(temporaryPath, path); err != nil {
		return fmt.Errorf("publish private state: %w", err)
	}
	return nil
}

func writePrivateTemporary(file *os.File, encoded []byte) error {
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return fmt.Errorf("protect private state: %w", err)
	}
	if _, err := file.Write(encoded); err != nil {
		file.Close()
		return fmt.Errorf("write private state: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync private state: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close private state: %w", err)
	}
	return nil
}
