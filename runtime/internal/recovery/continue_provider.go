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
	if source != nil {
		description = source.Description
		tags = state.CloneTags(source.Tags)
		if source.DisplayParentSessionID != nil {
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
		Description: description, Tags: tags, Kind: kind,
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
