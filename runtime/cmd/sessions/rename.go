package main

import (
	"fmt"
	"strings"
)

type renameResponse struct {
	Name string `json:"name"`
	Auto bool   `json:"auto,omitempty"`
}

func (a *app) cmdRename(args []string) error {
	if len(args) < 1 || strings.TrimSpace(args[0]) == "" {
		return fail(1, "usage: sessions rename <session> <name> | sessions rename <session> --auto")
	}
	id, err := a.resolveSessionID(args[0])
	if err != nil {
		return err
	}
	rest := args[1:]
	auto := false
	words := make([]string, 0, len(rest))
	for _, value := range rest {
		if value == "--auto" {
			auto = true
			continue
		}
		words = append(words, value)
	}
	name := strings.TrimSpace(strings.Join(words, " "))
	if auto && name != "" {
		return fail(1, "--auto follows the provider's own title, so it cannot be combined with a name")
	}
	if !auto && name == "" {
		return fail(1, "session name is required")
	}
	var result renameResponse
	if err := a.putJSON(
		"/api/sessions/"+escapeID(id)+"/name",
		renameResponse{Name: name, Auto: auto},
		&result,
		2,
	); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, result, true)
	}
	_, err = fmt.Fprintln(a.stdout, result.Name)
	return err
}
