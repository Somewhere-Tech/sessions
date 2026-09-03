package main

import "fmt"

func (a *app) cmdRetry(args []string) error {
	stop := removeFirst(&args, "--stop")
	if len(args) != 1 || args[0] == "" {
		return fail(1, "usage: sessions retry <id> [--stop]")
	}
	id, err := a.resolveSessionID(args[0])
	if err != nil {
		return err
	}
	path := "/api/sessions/" + escapeID(id) + "/retry"
	if stop {
		path += "/stop"
	}
	if err := a.postJSON(path, nil, nil, 2); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, struct {
			OK      bool   `json:"ok"`
			ID      string `json:"id"`
			Stopped bool   `json:"stopped"`
		}{true, id, stop}, false)
	}
	action := "retry started"
	if stop {
		action = "automatic retry stopped"
	}
	_, err = fmt.Fprintf(a.stdout, "%s for %s\n", action, id)
	return err
}
