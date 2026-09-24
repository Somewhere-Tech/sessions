package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"sort"
	"sync"
	"time"
)

const teamCursorTTL = 24 * time.Hour
const teamCursorMax = 128
const teamMemberMax = 512

type teamSnapshot struct {
	caller string
	at     time.Time
	rows   map[string][32]byte
}

// Bounded, content-free snapshots. A missing baseline is an explicit error,
// never an empty delta. These are observations, not a complete event history.
type teamChanges struct {
	mu        sync.Mutex
	snapshots map[string]teamSnapshot
}

var errTeamCursor = errors.New("the team cursor expired, belongs to another team, or predates a daemon restart; run sessions team again without --since to get a fresh baseline")

func memberFingerprint(member teamMember) [32]byte {
	// Terminal output timestamps change without useful new information. Keep
	// the status, compact result, receipt, and warnings in the comparison.
	member.UpdatedAt = 0
	encoded, _ := json.Marshal(member)
	return sha256.Sum256(encoded)
}

func (c *teamChanges) apply(caller, since string, listing *teamListing, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snapshots == nil {
		c.snapshots = make(map[string]teamSnapshot)
	}
	for key, value := range c.snapshots {
		if now.Sub(value.at) >= teamCursorTTL {
			delete(c.snapshots, key)
		}
	}
	previous, found := c.snapshots[since]
	if since != "" && (!found || previous.caller != caller) {
		return errTeamCursor
	}
	if len(listing.Members) > teamMemberMax {
		return errors.New("team exceeds 512 members; inspect a smaller manager's team")
	}
	current := teamSnapshot{caller: caller, at: now, rows: make(map[string][32]byte)}
	changed := make([]teamMember, 0)
	for _, member := range listing.Members {
		hash := memberFingerprint(member)
		current.rows[member.ID] = hash
		if old, seen := previous.rows[member.ID]; !seen || old != hash {
			changed = append(changed, member)
		}
	}
	listing.Removed = []string{}
	for id := range previous.rows {
		if _, present := current.rows[id]; !present {
			listing.Removed = append(listing.Removed, id)
		}
	}
	sort.Strings(listing.Removed)
	key, err := c.save(current)
	if err != nil {
		return err
	}
	listing.NextCursor, listing.Total = key, len(listing.Members)
	listing.Delta = since != ""
	if listing.Delta {
		listing.Members = changed
		listing.Self, listing.Parent = nil, nil
	}
	return nil
}

func (c *teamChanges) save(snapshot teamSnapshot) (string, error) {
	for key, existing := range c.snapshots {
		if existing.caller == snapshot.caller && maps.Equal(existing.rows, snapshot.rows) {
			c.snapshots[key] = snapshot
			return key, nil
		}
	}
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	if len(c.snapshots) >= teamCursorMax {
		oldest := ""
		for key, value := range c.snapshots {
			if oldest == "" || value.at.Before(c.snapshots[oldest].at) {
				oldest = key
			}
		}
		delete(c.snapshots, oldest)
	}
	key := hex.EncodeToString(entropy[:])
	c.snapshots[key] = snapshot
	return key, nil
}
