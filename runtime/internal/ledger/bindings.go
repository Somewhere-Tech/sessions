package ledger

import (
	"context"
	"fmt"
)

// LiveBinding identifies the lane which currently owns a provider
// conversation. Kind is the ledger's provider tool kind (claude-code/codex).
type LiveBinding struct {
	SessionID string `json:"sessionId"`
	Name      string `json:"name,omitempty"`
	Kind      string `json:"kind"`
}

// MovedConversation identifies the machine/endpoint to which a provider
// conversation was moved.
type MovedConversation struct {
	Machine         string `json:"machine"`
	SourceSessionID string `json:"-"`
}

// LiveBindingFor folds the append-only ledger and returns the current live
// owner of providerUUID. A forced rebound supersedes the old owner only after
// the replacement lane actually becomes managed-active, so a failed takeover
// launch cannot orphan the original binding.
func (s *Store) LiveBindingFor(ctx context.Context, providerUUID string) (*LiveBinding, error) {
	if !providerIDPattern.MatchString(providerUUID) {
		return nil, fmt.Errorf("live binding: invalid provider UUID %q", providerUUID)
	}
	events, err := s.Events(ctx, "")
	if err != nil {
		return nil, err
	}
	states := Fold(events)
	byID := make(map[string]LaneState, len(states))
	for _, state := range states {
		byID[state.LaneID] = state
	}
	var selected *LaneState
	for index := range states {
		candidate := &states[index]
		if candidate.ProviderUUID != providerUUID || !candidate.ManagedActive {
			continue
		}
		if replacement, ok := byID[candidate.ProviderReboundAs]; ok &&
			replacement.ManagedActive && replacement.ProviderUUID == providerUUID {
			continue
		}
		if selected == nil || candidate.CreatedAtMS > selected.CreatedAtMS ||
			(candidate.CreatedAtMS == selected.CreatedAtMS && candidate.LaneID > selected.LaneID) {
			selected = candidate
		}
	}
	if selected == nil {
		return nil, nil
	}
	name := selected.Name
	if name == "" {
		name = selected.LaneID
	}
	return &LiveBinding{SessionID: selected.LaneID, Name: name, Kind: selected.Tool}, nil
}

// MovedBinding returns the latest moved_to destination for providerUUID.
// Existing ledgers already store this as target_endpoint; the value is exposed
// as Machine so the collision warning remains stable if move gains a richer
// machine identity later.
func (s *Store) MovedBinding(ctx context.Context, providerUUID string) (*MovedConversation, error) {
	if !providerIDPattern.MatchString(providerUUID) {
		return nil, fmt.Errorf("moved binding: invalid provider UUID %q", providerUUID)
	}
	events, err := s.Events(ctx, "")
	if err != nil {
		return nil, err
	}
	states := Fold(events)
	byID := make(map[string]LaneState, len(states))
	for _, state := range states {
		byID[state.LaneID] = state
	}
	var selected *LaneState
	for index := range states {
		candidate := &states[index]
		if candidate.ProviderUUID != providerUUID || candidate.MovedToMachine == "" {
			continue
		}
		if replacement, ok := byID[candidate.ProviderReboundAs]; ok &&
			replacement.ManagedActive && replacement.ProviderUUID == providerUUID {
			continue
		}
		if selected == nil || candidate.MovedToSeq > selected.MovedToSeq ||
			(candidate.MovedToSeq == selected.MovedToSeq && candidate.LaneID > selected.LaneID) {
			selected = candidate
		}
	}
	if selected == nil {
		return nil, nil
	}
	return &MovedConversation{Machine: selected.MovedToMachine, SourceSessionID: selected.LaneID}, nil
}
