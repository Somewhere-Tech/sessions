package migrate

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func CreateRequestFromReceive(request ReceiveRequest) CreateRequest {
	return CreateRequest{
		Tool: request.Tool, UUID: request.UUID, Cwd: request.Cwd,
		ResumeRecipe: append([]string(nil), request.ResumeRecipe...),
		Name:         request.Name, SourceID: request.SourceID, SourceEndpoint: request.SourceEndpoint,
	}
}

func ValidateCreateRequest(request CreateRequest) error {
	if request.Cwd == "" || !filepath.IsAbs(request.Cwd) {
		return errors.New("cwd must be an absolute path")
	}
	if strings.TrimSpace(request.SourceID) == "" || strings.TrimSpace(request.SourceEndpoint) == "" {
		return errors.New("source_id and source_endpoint are required")
	}
	if !providerUUIDPattern.MatchString(request.UUID) {
		return errors.New("uuid is required for provider continuation")
	}
	tool := canonicalTool(request.Tool, firstArg(request.ResumeRecipe))
	if tool != string(state.ToolClaude) && tool != string(state.ToolCodex) {
		return errors.New("cross-machine continuation is only available for Claude and Codex conversations")
	}
	if len(request.ResumeRecipe) == 0 {
		return errors.New("resume_recipe is required")
	}
	provider, safe := ledger.SafeResumeRecipe(tool, request.ResumeRecipe[0], request.ResumeRecipe[1:])
	if provider != request.UUID || !slices.Equal(safe, request.ResumeRecipe) {
		return errors.New("resume_recipe is not the minimal recipe for uuid")
	}
	return nil
}

func SessionRequest(request CreateRequest) (state.CreateSessionRequest, error) {
	if err := ValidateCreateRequest(request); err != nil {
		return state.CreateSessionRequest{}, err
	}
	kind := state.KindCodexAppServer
	if canonicalTool(request.Tool, request.ResumeRecipe[0]) == string(state.ToolClaude) {
		kind = state.KindClaudeStructured
	}
	return state.CreateSessionRequest{
		Cmd: request.ResumeRecipe[0], Args: append([]string(nil), request.ResumeRecipe[1:]...),
		Cwd: request.Cwd, Name: request.Name, Kind: kind, ConversationID: request.UUID,
	}, nil
}

func firstArg(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
