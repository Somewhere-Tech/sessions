package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
)

// cmdRecall is intentionally a thin view over the versioned integration API.
// The HTTP contract, not this presentation layer, is the integration surface.
func (a *app) cmdRecall(args []string) error {
	raw := removeFirst(&args, "--raw")
	if len(args) > 1 || (raw && len(args) == 0) {
		return fail(1, "usage: sessions recall [<full-session-id> [--raw]]")
	}
	if len(args) == 0 {
		var history integrations.HistoryResponse
		if err := a.getJSON("/api/history", &history); err != nil {
			return err
		}
		if a.wantJSON {
			return writeJSON(a.stdout, history, true)
		}
		if len(history.Sessions) == 0 {
			_, err := io.WriteString(a.stdout, "(no history)\n")
			return err
		}
		for _, session := range history.Sessions {
			label := session.Name
			if strings.TrimSpace(label) == "" {
				label = session.CWD
			}
			availability := "available"
			if !session.ConversationAvailable {
				availability = "unavailable"
			}
			fmt.Fprintf(a.stdout, "%s  %s  %d messages  %s  %s\n",
				prefixString(session.ID, 8), session.Tool, session.MessageCount, availability, label)
		}
		return nil
	}

	id := args[0]
	if id == "" {
		return fail(1, "usage: sessions recall [<full-session-id> [--raw]]")
	}
	path := "/api/history/" + escapeID(id)
	if raw {
		if a.wantJSON {
			return fail(1, "--raw cannot be combined with --json")
		}
		path += "/raw"
	} else if a.wantJSON {
		path += "?format=json"
	} else {
		path += "?format=text"
	}
	response, err := a.api.request(context.Background(), http.MethodGet, path, nil, 0)
	if err != nil {
		return err
	}
	if response.status == http.StatusNotFound {
		return fail(1, "history session %s was not found", id)
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
	_, err = a.stdout.Write(response.body)
	return err
}
