package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

type profileSession struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type profileStatus struct {
	Tool     string           `json:"tool"`
	Name     string           `json:"name"`
	Path     string           `json:"path"`
	Label    string           `json:"label,omitempty"`
	SignedIn bool             `json:"signed_in"`
	Sessions []profileSession `json:"sessions"`
	LastUsed int64            `json:"last_used"`
}

// cmdAccounts is `sessions accounts`: the same list, plus the two verbs that
// make a second subscription a normal choice rather than a hidden one.
func (a *app) cmdAccounts(args []string) error {
	if len(args) == 0 {
		return a.cmdProfiles(nil)
	}
	switch args[0] {
	case "add":
		return a.cmdAccountsAdd(args[1:])
	case "forget", "remove":
		return a.cmdAccountsForget(args[1:])
	case "list":
		return a.cmdProfiles(args[1:])
	default:
		return fail(1, "usage: sessions accounts [list | add <name> --tool claude|codex [--label TEXT] | forget <name> --tool claude|codex]")
	}
}

// cmdAccountsAdd registers the account and opens the provider's own login in a
// session on the target machine. The login runs where the person can see it —
// Sessions prints what the provider prints and never handles the credential.
func (a *app) cmdAccountsAdd(args []string) error {
	tool, _ := pluck(&args, "--tool")
	label, _ := pluck(&args, "--label")
	if len(args) != 1 || args[0] == "" || (tool != "claude" && tool != "codex") {
		return fail(1, "usage: sessions accounts add <name> --tool claude|codex [--label TEXT]")
	}
	name := args[0]
	var created struct {
		Profile profileStatus `json:"profile"`
	}
	if err := a.postJSON("/api/profiles", map[string]string{"tool": tool, "name": name, "label": label}, &created, 2); err != nil {
		return err
	}
	command := "claude"
	loginArgs := []string{}
	if tool == "codex" {
		command, loginArgs = "codex", []string{"login"}
	}
	home, homeErr := os.UserHomeDir()
	if homeErr != nil {
		return fail(2, "resolve a working directory for the login session: %s", homeErr)
	}
	var info struct {
		ID string `json:"id"`
	}
	if err := a.postJSON("/api/sessions", createSessionRequest{
		Cmd: command, Args: loginArgs, Profile: name, Cwd: home,
		Name: "sign in: " + name, Description: "provider login for the " + tool + " account " + name,
	}, &info, 2); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, map[string]any{
			"account": created.Profile, "session": info.ID, "signed_in": created.Profile.SignedIn,
		}, true)
	}
	fmt.Fprintf(a.stdout, "account %s/%s registered at %s\n", tool, name, created.Profile.Path)
	fmt.Fprintf(a.stdout, "opened session %s to sign in\n", prefixString(info.ID, 8))
	if tool == "claude" {
		fmt.Fprintf(a.stdout, "run `sessions send %s /login` and follow what it prints; Sessions never sees the credential\n", prefixString(info.ID, 8))
	} else {
		fmt.Fprintf(a.stdout, "watch it with `sessions snap %s`: choose \"Sign in with ChatGPT\" and open the URL it prints\n", prefixString(info.ID, 8))
	}
	fmt.Fprintln(a.stdout, "check the account in the browser before confirming, then `sessions accounts` reports it signed in")
	return nil
}

func (a *app) cmdAccountsForget(args []string) error {
	tool, _ := pluck(&args, "--tool")
	if len(args) != 1 || args[0] == "" || (tool != "claude" && tool != "codex") {
		return fail(1, "usage: sessions accounts forget <name> --tool claude|codex")
	}
	var answer struct {
		Home string `json:"home"`
		Note string `json:"note"`
	}
	if err := a.deleteJSON("/api/profiles/"+tool+"/"+escapeID(args[0]), &answer); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, answer, true)
	}
	fmt.Fprintf(a.stdout, "%s/%s is no longer listed\n%s: %s\n", tool, args[0], answer.Note, answer.Home)
	return nil
}

func (a *app) cmdProfiles(args []string) error {
	if len(args) != 0 {
		return fail(1, "usage: sessions profiles (Sessions never deletes profile credentials; review the path from `sessions profiles` and remove it manually)")
	}
	var response struct {
		Profiles []profileStatus `json:"profiles"`
	}
	if err := a.getJSON("/api/profiles", &response); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, response.Profiles, true)
	}
	if len(response.Profiles) == 0 {
		_, err := io.WriteString(a.stdout, "(no profiles)\n")
		return err
	}
	rows := [][]string{{"TOOL", "NAME", "LABEL", "SIGNED-IN", "SESSIONS", "LAST-USED", "PATH"}}
	for _, profile := range response.Profiles {
		sessions := make([]string, 0, len(profile.Sessions))
		for _, current := range profile.Sessions {
			label := prefixString(current.ID, 8)
			if current.Name != "" {
				label = current.Name
			}
			sessions = append(sessions, label)
		}
		using := "-"
		if len(sessions) > 0 {
			using = strings.Join(sessions, ",")
		}
		lastUsed := "-"
		if profile.LastUsed > 0 {
			lastUsed = a.ageOf(profile.LastUsed)
		}
		label := profile.Label
		if label == "" {
			label = "-"
		}
		signedIn := "no"
		if profile.SignedIn {
			signedIn = "yes"
		}
		rows = append(rows, []string{profile.Tool, profile.Name, label, signedIn, using, lastUsed, profile.Path})
	}
	if err := writePaddedRows(a.stdout, rows); err != nil {
		return err
	}
	_, err := fmt.Fprintln(a.stdout, "Sessions never deletes profile credentials; remove one manually only after reviewing its PATH above.")
	return err
}
