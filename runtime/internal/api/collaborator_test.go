package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

func TestCollaboratorRouteImportsBriefingAndLeavesSourceUntouched(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	daemon := newTestDaemon(t)
	providerID := "11111111-2222-4333-8444-555555555555"
	source, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "claude", Args: []string{"--session-id", providerID}, ConversationID: providerID,
		Cwd: daemon.root, Kind: state.KindClaudeStructured, Name: "Manager",
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".claude", "projects", watch.EncodeClaudeCWD(daemon.root), providerID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"ORIGINAL CONTEXT"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	daemon.handler.registry = continuationCatalog(daemon.registry)
	body := `{"sourceSessionId":"` + source.ID + `","destinationProvider":"codex","contextMode":"briefing","briefing":"Only this reviewed brief","profile":""}`
	response := serve(t, daemon.handler, http.MethodPost, "/api/recovery/collaborator", strings.NewReader(body), "127.0.0.1:1", nil)
	if response.Code != http.StatusCreated {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	var result recovery.AdoptResult
	decodeBody(t, response, &result)
	if !result.SourceUntouched || result.ImportedMessages != 1 {
		t.Fatalf("%+v", result)
	}
	created, live := daemon.registry.Get(result.LaneID)
	if !live || created.Info().DisplayParentSessionID == nil || *created.Info().DisplayParentSessionID != "" {
		t.Fatal("collaborator is hidden under the source")
	}
	original, live := daemon.registry.Get(source.ID)
	if !live || original.Info().Exited || original.Info().ReopenedAs != "" {
		t.Fatal("source was changed")
	}
	stored, err := state.ReadContinuation(state.For(daemon.config.RunnerStateDir, result.LaneID).Continuation)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.BriefingOnly || len(stored.Messages) != 1 || stored.Messages[0].Text != "Only this reviewed brief" {
		t.Fatalf("unexpected context: %+v", stored)
	}
}

func TestCollaboratorRequestRequiresExplicitNewContract(t *testing.T) {
	profile := "second-account"
	valid := recoveryForkRequest{ContextMode: "briefing", Briefing: "Keep the source running.", Profile: &profile}
	if err := validateCollaboratorRequest("/api/recovery/collaborator", valid); err != nil {
		t.Fatal(err)
	}
	if err := validateCollaboratorRequest("/api/recovery/fork", valid); err == nil {
		t.Fatal("old fork route must not silently ignore briefing/account fields")
	}
	for _, body := range []recoveryForkRequest{
		{}, {ContextMode: "briefing"}, {ContextMode: "briefing", Briefing: strings.Repeat("x", maxBriefingBytes+1)},
	} {
		if err := validateCollaboratorRequest("/api/recovery/collaborator", body); err == nil {
			t.Fatalf("accepted invalid request: %+v", body)
		}
	}
	profile = "../other"
	if err := validateCollaboratorRequest("/api/recovery/collaborator", valid); err == nil {
		t.Fatal("accepted a profile path")
	}
}

func TestBriefingPromptPreservesSourceAndRefusesPartialSummary(t *testing.T) {
	conversation := state.ContinuationContext{Messages: []state.ContinuationMessage{
		{Role: "user", Text: "Do not message the PM."},
		{Role: "assistant", Text: "Tests not yet run."},
	}}
	prompt, err := briefingPrompt(conversation)
	if err != nil || !strings.Contains(prompt, "Do not message the PM.") || !strings.Contains(prompt, "Tests not yet run.") {
		t.Fatalf("prompt=%q err=%v", prompt, err)
	}
	conversation.Messages[0].Text = strings.Repeat("x", 256*1024)
	if prompt, err := briefingPrompt(conversation); err == nil || prompt != "" {
		t.Fatal("oversized conversation produced a partial briefing prompt")
	}
}
