package main

import (
	"fmt"
	"io"
	"net/url"
	"strings"
)

// Codes are accepted on stdin, not argv, so shell history and the process list
// never become an accidental OAuth-code log. Responses contain no credentials.
func (a *app) cmdAccountLogin(verb string, args []string) error {
	tool, _ := pluck(&args, "--tool")
	if len(args) != 1 {
		return fail(1, "usage: sessions accounts %s <profile-or-login-id> [--tool claude|codex]", verb)
	}
	var result map[string]any
	path := "/api/account-logins/" + url.PathEscape(args[0])
	var err error
	switch verb {
	case "login":
		err = a.postJSON("/api/account-logins", map[string]string{"tool": tool, "profile": args[0]}, &result, 2)
	case "login-status":
		err = a.getJSON(path, &result)
	case "login-code":
		code, readErr := io.ReadAll(io.LimitReader(a.stdin, 4097))
		if readErr != nil || len(code) > 4096 {
			return fail(1, "read a single provider confirmation code from stdin")
		}
		err = a.postJSON(path, map[string]string{"code": strings.TrimSpace(string(code))}, &result, 2)
	case "login-cancel":
		err = a.deleteJSON(path, &result)
	}
	if err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, result, true)
	}
	fmt.Fprintf(a.stdout, "sign-in %v: %v\n", result["id"], result["state"])
	for _, key := range []string{"url", "code", "message", "identity"} {
		if value, ok := result[key]; ok {
			fmt.Fprintf(a.stdout, "%s: %v\n", key, value)
		}
	}
	return nil
}
