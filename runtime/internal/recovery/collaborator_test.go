package recovery_test

import (
	"context"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestCollaboratorKeepsSourceAndChoosesDestinationAccount(t *testing.T) {
	for _, profile := range []string{"", "second-account"} {
		t.Run("profile="+profile, func(t *testing.T) {
			creator := &continuationCreator{}
			continuation := state.ContinuationContext{
				SchemaVersion: state.ContinuationBriefingSchemaVersion, SourceHistoryID: "source-session",
				SourceProvider: "codex", SourceProviderID: "original-provider", SourceCWD: t.TempDir(),
				DestinationProvider: "codex", DestinationProfile: &profile, Mode: state.ContinuationNativeImport,
				Fork: true, BriefingOnly: true, MainCollaborator: true,
				Messages: []state.ContinuationMessage{{Role: "user", Text: "Review the release; source stays running."}},
			}
			source := &recovery.AdoptSource{LaneID: "source-session", Profile: "original", ConfigDir: "/original-account", Tags: map[string]string{"project": "Sessions"}}
			result, err := recovery.ForkConversation(context.Background(), continuation, "Reviewer", creator, source)
			if err != nil {
				t.Fatal(err)
			}
			request := creator.request
			if !result.SourceUntouched || result.ForkedFromSessionID != source.LaneID || result.ImportedMessages != 1 {
				t.Fatalf("unexpected result: %+v", result)
			}
			if request.Profile != profile || request.ConfigDir != "" || request.ConversationID != "" {
				t.Fatalf("source account/identity leaked: %+v", request)
			}
			if request.DisplayParentSessionID == nil || *request.DisplayParentSessionID != "" || request.Tags["project"] != "Sessions" {
				t.Fatalf("not an independent project collaborator: %+v", request)
			}
			if source.Profile != "original" || source.ConfigDir != "/original-account" {
				t.Fatal("source changed")
			}
		})
	}
}
