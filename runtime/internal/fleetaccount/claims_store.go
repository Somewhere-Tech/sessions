package fleetaccount

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

const (
	claimReplayVersion    = 1
	maxClaimReplayBytes   = 1 << 20
	maxClaimReplayEntries = 4096
)

// One host daemon owns this store. Reload on every use so accepted claims also
// remain refused after daemon restart and fleet account logout/login.
type claimReplayStore struct {
	mu   sync.Mutex
	path string
}

type claimReplayDocument struct {
	Version int              `json:"version"`
	Expires map[string]int64 `json:"expires"`
}

func (s *claimReplayStore) consume(deviceID, nonce string, expires time.Time, clock func() time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := clock().UTC()
	if now.After(expires) {
		return ErrClaimExpired
	}
	document, err := s.load()
	if err != nil {
		return err
	}
	for key, seconds := range document.Expires {
		if now.After(time.Unix(seconds, 0)) {
			delete(document.Expires, key)
		}
	}
	tuple, _ := json.Marshal([2]string{deviceID, nonce})
	hash := sha256.Sum256(tuple)
	key := hex.EncodeToString(hash[:])
	if _, exists := document.Expires[key]; exists {
		return ErrClaimReplay
	}
	if len(document.Expires) >= maxClaimReplayEntries {
		return fmt.Errorf("%w: replay state is full; retry after accepted claims expire", ErrClaimStore)
	}
	document.Expires[key] = expires.Unix()
	if err := writePrivateJSON(s.path, document); err != nil {
		return fmt.Errorf("%w: could not save replay state: %v", ErrClaimStore, err)
	}
	return nil
}

func (s *claimReplayStore) load() (claimReplayDocument, error) {
	file, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return claimReplayDocument{Version: claimReplayVersion, Expires: map[string]int64{}}, nil
	}
	if err != nil {
		return claimReplayDocument{}, fmt.Errorf("%w: cannot read replay state", ErrClaimStore)
	}
	defer file.Close()
	encoded, err := io.ReadAll(io.LimitReader(file, maxClaimReplayBytes+1))
	if err != nil || len(encoded) > maxClaimReplayBytes {
		return claimReplayDocument{}, fmt.Errorf("%w: unreadable or oversized replay state", ErrClaimStore)
	}
	var document claimReplayDocument
	if err := json.Unmarshal(encoded, &document); err != nil || document.Version != claimReplayVersion ||
		document.Expires == nil || len(document.Expires) > maxClaimReplayEntries {
		return claimReplayDocument{}, fmt.Errorf("%w: invalid or unsupported replay state", ErrClaimStore)
	}
	for key, seconds := range document.Expires {
		decoded, err := hex.DecodeString(key)
		if err != nil || len(decoded) != sha256.Size || seconds <= 0 {
			return claimReplayDocument{}, fmt.Errorf("%w: invalid replay entry", ErrClaimStore)
		}
	}
	if err := file.Chmod(0o600); err != nil {
		return claimReplayDocument{}, fmt.Errorf("%w: cannot protect replay state", ErrClaimStore)
	}
	return document, nil
}
