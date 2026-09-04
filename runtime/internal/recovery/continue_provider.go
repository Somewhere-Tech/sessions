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

// ResumeFromTranscript creates a same-provider successor only when the
// provider-native handle is genuinely missing. It preserves the source record
// and authored history, and records the successor link like a native resume.
func ResumeFromTranscript(
	ctx context.Context,
	continuation state.ContinuationContext,
	name string,
	creator SessionCreator,
	observations ledger.ObservationWriter,
	source *AdoptSource,
) (AdoptResult, error) {
	continuation.TranscriptRecovery = true
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
	request, err := continuationCreateRequest(continuation, name, source, fork)
	if err != nil {
		return AdoptResult{}, err
	}
	created, err := creator.Create(ctx, request)
	if err != nil {
		return AdoptResult{}, err
	}
	result := AdoptResult{
		OK: true, LaneID: created.ID,
		SourceHistoryID:     continuation.SourceHistoryID,
		SourceProvider:      continuation.SourceProvider,
		DestinationProvider: continuation.DestinationProvider,
		Mode:                continuation.Mode, ImportedMessages: len(continuation.Messages),
		TranscriptRecovery: continuation.TranscriptRecovery,
		ForkPointIndex:     continuation.ForkPointIndex,
		ForkPointMessageID: continuation.ForkPointMessageID,
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

func continuationCreateRequest(
	continuation state.ContinuationContext,
	name string,
	source *AdoptSource,
	fork bool,
) (state.CreateSessionRequest, error) {
	cmd, kind := "", ""
	switch continuation.DestinationProvider {
	case "codex":
		cmd, kind = "codex", state.KindCodexAppServer
	case "claude":
		cmd, kind = "claude", state.KindClaudeStructured
	default:
		return state.CreateSessionRequest{}, fmt.Errorf(
			"unsupported destination provider %q", continuation.DestinationProvider,
		)
	}
	request := state.CreateSessionRequest{
		Cmd: cmd, Cwd: continuation.SourceCWD, Name: name, Kind: kind,
		Continuation: &continuation, Args: continuationArgs(continuation),
		Claude: continuationClaudeOptions(continuation),
	}
	if source == nil {
		return request, nil
	}
	request.Description = source.Description
	request.Tags = state.CloneTags(source.Tags)
	if (fork || continuation.TranscriptRecovery) && continuation.SourceProvider == continuation.DestinationProvider {
		request.Profile = source.Profile
		request.ConfigDir = source.ConfigDir
	}
	if fork && source.LaneID != "" {
		parent := source.LaneID
		request.DisplayParentSessionID = &parent
	} else if source.DisplayParentSessionID != nil {
		parent := *source.DisplayParentSessionID
		request.DisplayParentSessionID = &parent
	}
	return request, nil
}

func continuationArgs(continuation state.ContinuationContext) []string {
	model := strings.TrimSpace(continuation.DestinationModel)
	effort := strings.TrimSpace(continuation.DestinationEffort)
	if continuation.DestinationProvider != "codex" {
		return nil
	}
	args := make([]string, 0, 4)
	if model != "" {
		args = append(args, "--model", model)
	}
	if effort != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%s", effort))
	}
	return args
}

func continuationClaudeOptions(continuation state.ContinuationContext) *state.ClaudeSessionOptions {
	if continuation.DestinationProvider != "claude" {
		return nil
	}
	model := strings.TrimSpace(continuation.DestinationModel)
	effort := strings.TrimSpace(continuation.DestinationEffort)
	if model == "" && effort == "" {
		return nil
	}
	return &state.ClaudeSessionOptions{Model: model, Effort: effort}
}
