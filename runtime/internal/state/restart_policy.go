package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const DefaultPinnedBootRestoreLimit = 8

type RestartPermit struct {
	BootID      string `json:"boot_id"`
	CreatedAtMS int64  `json:"created_at_ms"`
}

type RestorePending struct {
	SessionID    string `json:"session_id"`
	Reason       string `json:"reason"`
	DetectedAtMS int64  `json:"detected_at_ms"`
}

type RestartDecision struct {
	Allowed       bool
	PinnedRestore bool
	Reason        string
}

func WriteRestartPermit(path, bootID string) error {
	if bootID == "" {
		return errors.New("boot id is required")
	}
	return writeProtectedJSON(path, RestartPermit{BootID: bootID, CreatedAtMS: time.Now().UnixMilli()})
}

// EvaluateRestartPermit is the runner-side reboot boundary. The permit remains
// present while a runner is live, so launchd can restart a crash during the
// same boot. On a new boot, only a bounded set of the most recently active
// pinned session roots is allowed to respawn; every other provider stays
// stopped and receives a recoverable marker instead.
func EvaluateRestartPermit(paths Paths, currentBootID string, pinnedLimit int) (RestartDecision, error) {
	if currentBootID == "" {
		return RestartDecision{Reason: "Sessions could not identify this boot, so it did not restart a provider automatically"}, errors.New("boot id is required")
	}
	permit, err := readRestartPermit(paths.KeepAlive)
	if err != nil {
		reason := "no same-boot runner permit exists"
		if !errors.Is(err, os.ErrNotExist) {
			reason = "the runner permit is unreadable"
		}
		return RestartDecision{Reason: reason}, err
	}
	if permit.BootID == currentBootID {
		_ = os.Remove(paths.RestorePending)
		return RestartDecision{Allowed: true}, nil
	}
	if pinnedLimit <= 0 {
		pinnedLimit = DefaultPinnedBootRestoreLimit
	}
	eligible, scanErr := pinnedRestoreIDs(paths.Dir, pinnedLimit)
	if scanErr == nil {
		index := sort.SearchStrings(eligible, paths.ID)
		if index < len(eligible) && eligible[index] == paths.ID {
			if err := WriteRestartPermit(paths.KeepAlive, currentBootID); err != nil {
				return RestartDecision{Reason: "could not renew the pinned-session restart permit"}, err
			}
			_ = os.Remove(paths.RestorePending)
			return RestartDecision{Allowed: true, PinnedRestore: true}, nil
		}
	}
	reason := fmt.Sprintf("automatic restore paused after reboot; only the %d most recently active pinned session roots restart automatically", pinnedLimit)
	if scanErr != nil {
		reason = "automatic restore paused after reboot because Sessions could not safely select pinned roots: " + scanErr.Error()
	}
	if err := os.Remove(paths.KeepAlive); err != nil && !errors.Is(err, os.ErrNotExist) {
		return RestartDecision{Reason: reason}, fmt.Errorf("remove stale runner permit: %w", err)
	}
	if err := WriteRestorePending(paths.RestorePending, paths.ID, reason); err != nil {
		return RestartDecision{Reason: reason}, fmt.Errorf("record paused restore: %w", err)
	}
	return RestartDecision{Reason: reason}, nil
}

func readRestartPermit(path string) (RestartPermit, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return RestartPermit{}, err
	}
	var permit RestartPermit
	if err := json.Unmarshal(encoded, &permit); err != nil {
		return RestartPermit{}, err
	}
	if permit.BootID == "" {
		return RestartPermit{}, errors.New("permit has no boot id")
	}
	return permit, nil
}

func WriteRestorePending(path, sessionID, reason string) error {
	return writeProtectedJSON(path, RestorePending{
		SessionID: sessionID, Reason: reason, DetectedAtMS: time.Now().UnixMilli(),
	})
}

// ReadRestorePending decodes the durable proof that a runner deliberately did
// not restart on this boot. Callers must not treat the marker as an empty or
// ended session: the provider process is unavailable and needs an explicit
// resume/recreate action.
func ReadRestorePending(path string) (RestorePending, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return RestorePending{}, err
	}
	var pending RestorePending
	if err := json.Unmarshal(encoded, &pending); err != nil {
		return RestorePending{}, err
	}
	if strings.TrimSpace(pending.SessionID) == "" {
		return RestorePending{}, errors.New("restore marker has no session id")
	}
	return pending, nil
}

// CountRestorePending returns the number of runners intentionally left stopped
// by the reboot budget. An unreadable marker still counts: it is evidence that
// automatic restoration did not complete, even if its detail cannot be read.
func CountRestorePending(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".restore-pending.json") {
			count++
		}
	}
	return count, nil
}

func pinnedRestoreIDs(dir string, limit int) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		id       string
		activity int64
	}
	candidates := make([]candidate, 0)
	for _, entry := range entries {
		id, ok := RunnerIDFromMetadataName(entry.Name())
		if !ok {
			continue
		}
		metadata, err := ReadRunnerMetadata(filepath.Join(dir, entry.Name()))
		if err != nil || !metadata.Pinned || metadata.Kind == KindLane {
			continue
		}
		// A pinned but already-stopped session must not consume one of the
		// bounded restore slots. The permit is the durable proof that this
		// runner was actually alive before the reboot.
		if _, err := readRestartPermit(For(dir, id).KeepAlive); err != nil {
			continue
		}
		activity := metadata.Info.CreatedAt
		for _, stamp := range []*int64{metadata.LastHumanMessageAt, metadata.LastAgentMessageAt} {
			if stamp != nil && *stamp > activity {
				activity = *stamp
			}
		}
		candidates = append(candidates, candidate{id: id, activity: activity})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].activity == candidates[j].activity {
			return candidates[i].id < candidates[j].id
		}
		return candidates[i].activity > candidates[j].activity
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	ids := make([]string, len(candidates))
	for index, candidate := range candidates {
		ids[index] = candidate.id
	}
	// EvaluateRestartPermit uses binary search because every runner evaluates
	// independently. Sort only after selecting by activity.
	sort.Strings(ids)
	return ids, nil
}

func writeProtectedJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(encoded, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(temporary, 0o600); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}
