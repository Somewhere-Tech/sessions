package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
)

func (a *app) cmdCat(args []string) error {
	if len(args) != 1 || args[0] == "" {
		return fail(1, "usage: sessions cat <machine::history-id>")
	}
	reference := args[0]
	if _, err := a.useQualifiedHistoryReference(&reference); err != nil {
		return err
	}
	path := "/api/history/" + escapeID(reference)
	if a.wantJSON {
		path += "?format=json"
	} else {
		path += "?format=text"
	}
	response, err := a.api.request(context.Background(), http.MethodGet, path, nil, 0)
	if err != nil {
		return err
	}
	if response.status == http.StatusNotFound {
		return fail(1, "history conversation %s was not found", reference)
	}
	if response.status >= 400 {
		return fail(2, "%s → %d %s", path, response.status, prefixBytes(response.body, 200))
	}
	if a.wantJSON {
		var transcript integrations.TranscriptResponse
		if err := json.Unmarshal(response.body, &transcript); err != nil {
			return err
		}
		return writeJSON(a.stdout, transcript, true)
	}
	if _, err := a.stdout.Write(response.body); err != nil {
		return fmt.Errorf("write transcript: %w", err)
	}
	return nil
}
