package main

import (
	"fmt"
	"io"
)

func (a *app) cmdToken() error {
	token, err := a.api.readToken()
	if err != nil {
		return fail(2, "%s", err)
	}
	if token == "" {
		fmt.Fprintf(a.stderr, "sessions: no token found at %s\n", a.api.tokenPath)
		io.WriteString(a.stderr, "        start the daemon first (or run: sessions install), then retry.\n")
		return status(1)
	}
	_, err = fmt.Fprintln(a.stdout, token)
	return err
}
