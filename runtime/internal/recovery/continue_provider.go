package recovery

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// ContinueAcrossProviders creates a new destination-provider conversation
// through the normal durable session boundary. The source provider history is
// read-only and remains intact. A source Sessions runtime, when supplied, is
// linked only after the destination runner has started successfully.
func ContinueAcrossProviders(
	ctx context.Context,
	continuation state.ContinuationContext,
	name string,
	creator SessionCreator,
	observations ledger.ObservationWriter,
	source *AdoptSource,
) (AdoptResult, error) {
	return createProviderCopy(ctx, continuation, name, creator, observations, source, false)
}

// ForkConversation creates a new Rich conversation from one stable authored
// history snapshot. The source runtime remains live and is never marked as
// reopened or superseded. Display hierarchy records the new conversation as a
// child of the source so the branch is visible without rewriting provenance.
func ForkConversation(
	ctx context.Context,
	continuation state.ContinuationContext,
	name string,
	creator SessionCreator,
	source *AdoptSource,
) (AdoptResult, error) {
	continuation.Fork = true
	return createProviderCopy(ctx, continuation, name, creator, nil, source, true)
}

func createProviderCopy(
	ctx context.Context,
	continuation state.ContinuationContext,
	name string,
	creator SessionCreator,
	observations ledger.ObservationWriter,
	source *AdoptSource,
	fork bool,
) (AdoptResult, error) {
	if err := continuation.Validate(); err != nil {
		return AdoptResult{}, err
	}
	if creator == nil {
		return AdoptResult{}, errors.New("session creator is unavailable")
	}
	if strings.TrimSpace(name) == "" {
		name = strings.TrimSpace(continuation.SourceTitle)
	}
	if strings.TrimSpace(name) == "" {
		name = filepath.Base(continuation.SourceCWD)
	}
	description := ""
	var tags map[string]string
	var displayParent *string
	profile := ""
	configDir := ""
	if source != nil {
		description = source.Description
		tags = state.CloneTags(source.Tags)
		if fork && continuation.SourceProvider == continuation.DestinationProvider {
			profile = source.Profile
			configDir = source.ConfigDir
		}
		if fork && source.LaneID != "" {
			parent := source.LaneID
			displayParent = &parent
		} else if source.DisplayParentSessionID != nil {
			parent := *source.DisplayParentSessionID
			displayParent = &parent
		}
	}
	cmd, kind := "", ""
	switch continuation.DestinationProvider {
	case "codex":
		cmd, kind = "codex", state.KindCodexAppServer
	case "claude":
		cmd, kind = "claude", state.KindClaudeStructured
	default:
		return AdoptResult{}, fmt.Errorf(
			"unsupported destination provider %q", continuation.DestinationProvider,
		)
	}
	created, err := creator.Create(ctx, state.CreateSessionRequest{
		Cmd: cmd, Cwd: continuation.SourceCWD, Name: name,
		Description: description, Tags: tags, Kind: kind, Profile: profile,
		ConfigDir:              configDir,
		DisplayParentSessionID: displayParent, Continuation: &continuation,
	})
	if err != nil {
		return AdoptResult{}, err
	}
	result := AdoptResult{
		OK: true, LaneID: created.ID,
		SourceHistoryID:     continuation.SourceHistoryID,
		SourceProvider:      continuation.SourceProvider,
		DestinationProvider: continuation.DestinationProvider,
		Mode:                continuation.Mode, ImportedMessages: len(continuation.Messages),
	}
	if fork {
		result.SourceUntouched = true
		if source != nil {
			result.ForkedFromSessionID = source.LaneID
		}
		return result, nil
	}
	if source != nil && source.LaneID != "" {
		if observations == nil {
			result.OK = false
			result.Partial = true
			result.Warning = "The new conversation is live, but Sessions could not record its source-session link."
			return result, nil
		}
		if err := observations.RecordReopened(ctx, ledger.Reopened{
			Meta: ledger.Meta{LaneID: source.LaneID}, NewLaneID: created.ID,
		}); err != nil {
			result.OK = false
			result.Partial = true
			result.Warning = "The new conversation is live, but Sessions could not record its source-session link: " + err.Error()
		}
	}
	return result, nil
}
