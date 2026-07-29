package recovery_test

import (
	"context"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type continuationCreator struct {
	request state.CreateSessionRequest
}

func (c *continuationCreator) Create(_ context.Context, request state.CreateSessionRequest) (state.SessionInfo, error) {
	c.request = request
	return state.SessionInfo{ID: "destination-lane"}, nil
}

func TestContinueAcrossProvidersCreatesFreshDestinationWithoutCredentials(t *testing.T) {
	creator := &continuationCreator{}
	continuation := state.ContinuationContext{
		SchemaVersion:   state.ContinuationSchemaVersion,
		SourceHistoryID: "provider:claude:source", SourceProvider: "claude",
		SourceProviderID: "source", SourceTitle: "Authentication review", SourceCWD: t.TempDir(),
		DestinationProvider: "codex", Mode: state.ContinuationNativeImport,
		Messages: []state.ContinuationMessage{
			{Role: "user", Text: "Review auth."},
			{Role: "assistant", Text: "I found the issue."},
		},
	}
	result, err := recovery.ContinueAcrossProviders(
		context.Background(), continuation, "", creator, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.LaneID != "destination-lane" || result.ImportedMessages != 2 {
		t.Fatalf("result = %+v", result)
	}
	if creator.request.Cmd != "codex" || creator.request.Kind != state.KindCodexAppServer {
		t.Fatalf("destination request = %+v", creator.request)
	}
	if creator.request.Profile != "" || creator.request.ConfigDir != "" ||
		creator.request.ConversationID != "" {
		t.Fatalf("source credentials or provider identity leaked into destination: %+v", creator.request)
	}
	if creator.request.Continuation == nil ||
		creator.request.Continuation.SourceHistoryID != continuation.SourceHistoryID {
		t.Fatalf("continuation was not carried through create: %+v", creator.request.Continuation)
	}
}
