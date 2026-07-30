package main

import (
	"fmt"
	"strings"
)

type renameResponse struct {
	Name string `json:"name"`
}

func (a *app) cmdRename(args []string) error {
	if len(args) < 2 || strings.TrimSpace(args[0]) == "" {
		return fail(1, "usage: sessions rename <session> <name>")
	}
	id, err := a.resolveSessionID(args[0])
	if err != nil {
		return err
	}
	name := strings.TrimSpace(strings.Join(args[1:], " "))
	if name == "" {
		return fail(1, "session name is required")
	}
	var result renameResponse
	if err := a.putJSON(
		"/api/sessions/"+escapeID(id)+"/name",
		renameResponse{Name: name},
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
