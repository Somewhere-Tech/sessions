//go:build windows

package state

import (
	"errors"
	"os"
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
		if entry.IsDir() {
			continue
		}
		// Windows has no socket artifact to key off, so metadata names are the
		// discovery key. RunnerIDFromMetadataName owns the sidecar exclusions
		// so this cannot drift behind a new Paths field.
		id, ok := RunnerIDFromMetadataName(entry.Name())
		if !ok {
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
}
