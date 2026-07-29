//go:build !windows

package state

import (
	"errors"
	"os"
	"strings"
)

func RunnerArtifactIDs(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sock") {
			ids = append(ids, strings.TrimSuffix(entry.Name(), ".sock"))
		}
	}
	return ids, nil
}
