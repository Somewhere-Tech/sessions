package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
	"github.com/somewhere-tech/sessions/runtime/internal/providerargs"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func listLiveCodexModels(ctx context.Context, codexPath string) ([]codexapp.Model, error) {
	options := codexapp.Options{CodexPath: codexPath}
	if socketPath := strings.TrimSpace(os.Getenv("SESSIONS_CODEX_APP_SERVER_SOCKET")); socketPath != "" {
		options.SkipDaemonStart = true
		options.SocketPath = socketPath
	}
	client, err := codexapp.NewClient(ctx, options)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	return client.ListModels(ctx)
}

func (m *Manager) resolveCodexModelChoice(
	ctx context.Context,
	request state.CreateSessionRequest,
) (state.CreateSessionRequest, error) {
	if strings.TrimSpace(request.Kind) != state.KindCodexAppServer {
		return request, nil
	}
	if strings.ToLower(filepath.Base(request.Cmd)) != "codex" {
		return request, nil
	}
	choice := codexModelChoice(request.Args)
	if choice.Model == "" && choice.Effort == "" && choice.ServiceTier == "" {
		return request, nil
	}
	catalog, err := m.listModels(ctx, request.Cmd)
	if err != nil {
		return request, fmt.Errorf("load live Codex model catalog: %w", err)
	}
	resolved, err := codexapp.ResolveModelChoice(catalog, choice)
	if err != nil {
		return request, err
	}
	request.Args = withCodexModel(request.Args, resolved.Model)
	return request, nil
}

func codexModelChoice(args []string) codexapp.ModelChoice {
	return codexapp.ModelChoice{
		Model:       providerargs.Value(args, providerargs.ModelFlags()...),
		Effort:      providerargs.ConfigValue(args, providerargs.CodexEffortKey),
		ServiceTier: providerargs.ConfigValue(args, providerargs.CodexServiceTierKey),
	}
}

func withCodexModel(args []string, model string) []string {
	return providerargs.WithValue(args, model, providerargs.ModelFlags()...)
}
