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

type sourceCLIView struct {
	Reference    string                     `json:"reference"`
	MachineAlias string                     `json:"machine_alias"`
	Machine      string                     `json:"machine"`
	Source       integrations.HistorySource `json:"source"`
}

func (a *app) cmdSource(args []string) error {
	raw := removeFirst(&args, "--raw")
	textMode := removeFirst(&args, "--text")
	if raw && textMode {
		return fail(1, "--raw and --text cannot be combined")
	}
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return fail(1, "usage: sessions source <[machine::]name-or-id> [--text | --raw]")
	}
	if raw && a.wantJSON {
		return fail(1, "--raw cannot be combined with --json")
	}
	resolution, err := a.resolveHistoryReference(args[0])
	if err != nil {
		return err
	}
	reference := resolution.Reference
	alias, err := a.useQualifiedHistoryReference(&reference)
	if err != nil {
		return err
	}
	if alias != "" {
		resolution.Alias = alias
	}
	id := reference
	if textMode || raw {
		path := "/api/history/" + escapeID(id)
		if raw {
			path += "/raw"
		} else {
			path += "?format=text"
		}
		response, requestErr := a.api.request(context.Background(), http.MethodGet, path, nil, 0)
		if requestErr != nil {
			return requestErr
		}
		if response.status == http.StatusNotFound {
			return fail(1, "saved conversation %s was not found", resolution.Reference)
		}
		if response.status >= 400 {
			return fail(2, "%s → %d %s", path, response.status, prefixBytes(response.body, 200))
		}
		_, err = a.stdout.Write(response.body)
		return err
	}

	path := "/api/history/" + escapeID(id) + "/source"
	response, err := a.api.request(context.Background(), http.MethodGet, path, nil, 0)
	if err != nil {
		return err
	}
	if response.status == http.StatusNotFound {
		return fail(1, "saved conversation %s was not found", resolution.Reference)
	}
	if response.status >= 400 {
		return fail(2, "%s → %d %s", path, response.status, prefixBytes(response.body, 200))
	}
	var source integrations.HistorySource
	if err := json.Unmarshal(response.body, &source); err != nil {
		return err
	}
	view := sourceCLIView{
		Reference: resolution.Reference, MachineAlias: resolution.Alias,
		Machine: resolution.Machine, Source: source,
	}
	if a.wantJSON {
		return writeJSON(a.stdout, view, true)
	}
	fmt.Fprintf(a.stdout, "Conversation  %s\n", source.Session.Name)
	fmt.Fprintf(a.stdout, "Reference     %s\n", resolution.Reference)
	fmt.Fprintf(a.stdout, "Provider      %s\n", source.Session.Tool)
	if resolution.Machine != "" {
		fmt.Fprintf(a.stdout, "Machine       %s (%s)\n", resolution.Machine, resolution.Alias)
	}
	fmt.Fprintf(a.stdout, "Workspace     %s\n", source.Session.CWD)
	fmt.Fprintf(a.stdout, "Source        %s\n", source.SourcePath)
	fmt.Fprintf(a.stdout, "Source kind   %s\n", source.SourceKind)
	fmt.Fprintf(a.stdout, "Raw bytes     %d\n", source.RawBytes)
	if source.MirrorDamaged {
		// Placed above the read recipes on purpose: the person about to run one
		// of them is the one who needs to know that what comes back is part of
		// the conversation rather than the whole of it.
		fmt.Fprintf(a.stdout, "Damaged       %s.\n", source.MirrorDetail)
		io.WriteString(a.stdout,
			"              Reading this conversation gives you the part Sessions stored, not all of it, and the rest "+
				"cannot be recovered from here. If the provider still holds its own transcript, that copy is the complete one.\n")
	}
	if source.TextAvailable {
		fmt.Fprintf(a.stdout, "Read text     sessions source %s --text\n", shellRecipe([]string{resolution.Reference}))
	}
	if source.RawAvailable {
		fmt.Fprintf(a.stdout, "Read raw      sessions source %s --raw\n", shellRecipe([]string{resolution.Reference}))
	}
	return nil
}
