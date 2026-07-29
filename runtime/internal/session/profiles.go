package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type ProfileSession struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type ProfileStatus struct {
	Tool     string           `json:"tool"`
	Name     string           `json:"name"`
	Path     string           `json:"path"`
	Sessions []ProfileSession `json:"sessions"`
	LastUsed int64            `json:"last_used"`
}

func (m *Manager) prepareProfile(command, name string) (string, error) {
	if err := state.ValidateProfileName(name); err != nil {
		return "", err
	}
	if m.config.UserStateRoot == "" {
		return "", errors.New("--profile requires a configured Sessions user state root")
	}
	tool, supported := state.ProfileToolName(state.CommandTool(command))
	if !supported {
		return "", errors.New("--profile is only for Claude or Codex sessions; remove it for shell sessions")
	}
	path := filepath.Join(m.config.UserStateRoot, "profiles", tool, name)
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", fmt.Errorf("create profile directory %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return "", fmt.Errorf("make profile directory private %s: %w", path, err)
	}
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		return "", fmt.Errorf("mark profile used %s: %w", path, err)
	}
	return path, nil
}

func (m *Manager) Profiles(ctx context.Context) ([]ProfileStatus, error) {
	if m.config.UserStateRoot == "" {
		return nil, errors.New("profile listing requires a configured Sessions user state root")
	}
	profiles := make(map[string]*ProfileStatus)
	root := filepath.Join(m.config.UserStateRoot, "profiles")
	for _, tool := range []string{"claude", "codex"} {
		entries, err := os.ReadDir(filepath.Join(root, tool))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("list %s profiles: %w", tool, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() || state.ValidateProfileName(entry.Name()) != nil {
				continue
			}
			path := filepath.Join(root, tool, entry.Name())
			lastUsed := int64(0)
			if info, statErr := entry.Info(); statErr == nil {
				lastUsed = info.ModTime().UnixMilli()
			}
			key := tool + "\x00" + entry.Name()
			profiles[key] = &ProfileStatus{
				Tool: tool, Name: entry.Name(), Path: path,
				Sessions: make([]ProfileSession, 0), LastUsed: lastUsed,
			}
		}
	}
	states, err := m.ledgerStates(ctx)
	if err != nil {
		return nil, fmt.Errorf("read profile usage: %w", err)
	}
	for _, lane := range states {
		tool, supported := state.ProfileToolName(state.SessionTool(lane.Tool))
		if !supported || lane.Profile == "" {
			continue
		}
		key := tool + "\x00" + lane.Profile
		profile := profiles[key]
		if profile == nil {
			continue
		}
		if lane.CreatedAtMS > profile.LastUsed {
			profile.LastUsed = lane.CreatedAtMS
		}
	}
	for _, info := range m.registry.List(false) {
		if info.Profile == "" || info.Exited {
			continue
		}
		tool, supported := state.ProfileToolName(info.Tool)
		if !supported {
			continue
		}
		if profile := profiles[tool+"\x00"+info.Profile]; profile != nil {
			profile.Sessions = append(profile.Sessions, ProfileSession{ID: info.ID, Name: info.Name})
		}
	}
	result := make([]ProfileStatus, 0, len(profiles))
	for _, profile := range profiles {
		sort.Slice(profile.Sessions, func(i, j int) bool { return profile.Sessions[i].ID < profile.Sessions[j].ID })
		result = append(result, *profile)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Tool != result[j].Tool {
			return result[i].Tool < result[j].Tool
		}
		return result[i].Name < result[j].Name
	})
	return result, nil
}
