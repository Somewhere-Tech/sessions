package migrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

func ResolveSource(ctx context.Context, store *ledger.Store, source SourceSession) (ReceiveRequest, error) {
	if store == nil {
		return ReceiveRequest{}, fmt.Errorf("resolve source: ledger is required")
	}
	if source.ID == "" {
		return ReceiveRequest{}, fmt.Errorf("resolve source: session id is required")
	}
	events, err := store.Events(ctx, source.ID)
	if err != nil {
		return ReceiveRequest{}, fmt.Errorf("resolve source ledger: %w", err)
	}
	states := ledger.Fold(events)
	if len(states) != 1 || !states[0].Created {
		return ReceiveRequest{}, fmt.Errorf("resolve source: session %s has no creation record in the local ledger", source.ID)
	}
	lane := states[0]
	tool := canonicalTool(source.Tool, source.Cmd)
	providerUUID := lane.ProviderUUID
	resumeRecipe := append([]string(nil), lane.ResumeArgv...)
	if providerUUID == "" || len(resumeRecipe) == 0 {
		providerUUID, resumeRecipe = ledger.SafeResumeRecipe(tool, source.Cmd, source.Args)
	}

	request := ReceiveRequest{
		Tool: tool, UUID: providerUUID, Cwd: source.Cwd,
		ResumeRecipe: resumeRecipe, Name: source.Name, SourceID: source.ID,
	}
	switch tool {
	case "claude-code":
		if providerUUID == "" || len(resumeRecipe) == 0 {
			return ReceiveRequest{}, fmt.Errorf("resolve source: Claude provider identity is not bound yet")
		}
		projects, err := watch.ClaudeProjectsDir()
		if err != nil {
			return ReceiveRequest{}, fmt.Errorf("resolve Claude store: %w", err)
		}
		resolved := watch.ResolveClaudeCWD(projects, source.Cwd, providerUUID)
		if resolved.Path == "" {
			return ReceiveRequest{}, fmt.Errorf("resolve source: Claude conversation not found (%s)", resolved.Reason)
		}
		request.ConversationBytes, err = readConversation(resolved.Path)
		if err != nil {
			return ReceiveRequest{}, err
		}
	case "codex":
		if providerUUID == "" || len(resumeRecipe) == 0 {
			return ReceiveRequest{}, fmt.Errorf("resolve source: Codex provider identity is not bound yet")
		}
		resolved := watch.ResolveCodexRolloutPath(watch.CodexResolveOptions{
			CWD: source.Cwd, Args: resumeRecipe[1:], CreatedAt: time.UnixMilli(source.CreatedAt),
		})
		if resolved.Path == "" {
			return ReceiveRequest{}, fmt.Errorf("resolve source: Codex conversation not found (%s)", resolved.Reason)
		}
		request.ConversationBytes, err = readConversation(resolved.Path)
		if err != nil {
			return ReceiveRequest{}, err
		}
	default:
		if source.Cmd == "" {
			return ReceiveRequest{}, fmt.Errorf("resolve source: terminal session has no command")
		}
		request.UUID = ""
		request.ResumeRecipe = append([]string{source.Cmd}, source.Args...)
	}
	return request, nil
}

func canonicalTool(tool, cmd string) string {
	base := strings.ToLower(filepath.Base(cmd))
	switch {
	case tool == "claude-code" || base == "claude":
		return "claude-code"
	case tool == "codex" || base == "codex":
		return "codex"
	case tool != "":
		return tool
	default:
		return "terminal"
	}
}

func readConversation(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat conversation: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("conversation is not a regular file: %s", path)
	}
	if info.Size() > MaxConversationBytes {
		return nil, fmt.Errorf("conversation is %d bytes; maximum move size is %d", info.Size(), MaxConversationBytes)
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read conversation: %w", err)
	}
	if len(encoded) == 0 {
		return nil, fmt.Errorf("conversation is empty: %s", path)
	}
	return encoded, nil
}
