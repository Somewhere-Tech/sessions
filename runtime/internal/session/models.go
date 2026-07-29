package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
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
		Model:       codexArgValue(args, "--model", "-m"),
		Effort:      codexConfigValue(args, "model_reasoning_effort"),
		ServiceTier: codexConfigValue(args, "service_tier"),
	}
}

func codexArgValue(args []string, names ...string) string {
	for index := 0; index+1 < len(args); index++ {
		for _, name := range names {
			if args[index] == name {
				return strings.TrimSpace(args[index+1])
			}
		}
	}
	return ""
}

func codexConfigValue(args []string, key string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] != "-c" && args[index] != "--config" {
			continue
		}
		if value, ok := strings.CutPrefix(args[index+1], key+"="); ok {
			return strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return ""
}

func withCodexModel(args []string, model string) []string {
	result := append([]string(nil), args...)
	for index := 0; index+1 < len(result); index++ {
		if result[index] == "--model" || result[index] == "-m" {
			result[index+1] = model
			return result
		}
	}
	return append(result, "--model", model)
}
