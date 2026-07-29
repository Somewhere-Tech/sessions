package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
)

func listLiveCodexModels(ctx context.Context) ([]codexapp.Model, error) {
	options := codexapp.Options{}
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

func (a *app) cmdModels(args []string) error {
	if len(args) != 0 {
		return fail(1, "usage: sessions models [--json]")
	}
	catalog, err := a.listModels(context.Background())
	if err != nil {
		return fail(2, "%s", err)
	}
	if a.wantJSON {
		return writeJSON(a.stdout, catalog, true)
	}
	writer := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "MODEL\tDISPLAY NAME\tDEFAULT MODEL\tDEFAULT EFFORT\tHIDDEN\tEFFORTS\tSERVICE TIERS"); err != nil {
		return err
	}
	for _, model := range catalog {
		isDefault := ""
		if model.IsDefault {
			isDefault = "yes"
		}
		hidden := ""
		if model.Hidden {
			hidden = "yes"
		}
		efforts := make([]string, 0, len(model.SupportedReasoningEfforts))
		for _, option := range model.SupportedReasoningEfforts {
			efforts = append(efforts, option.ReasoningEffort)
		}
		tiers := make([]string, 0, len(model.ServiceTiers))
		for _, option := range model.ServiceTiers {
			tiers = append(tiers, option.ID)
		}
		if len(tiers) == 0 {
			tiers = append(tiers, "-")
		}
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", model.ID, model.DisplayName, isDefault,
			model.DefaultReasoningEffort, hidden,
			strings.Join(efforts, ","), strings.Join(tiers, ",")); err != nil {
			return err
		}
	}
	return writer.Flush()
}
