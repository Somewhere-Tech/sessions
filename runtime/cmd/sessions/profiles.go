package main

import (
	"fmt"
	"io"
	"strings"
)

type profileSession struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type profileStatus struct {
	Identity *struct {
		Email        string `json:"email"`
		Plan         string `json:"plan,omitempty"`
		Organization string `json:"organization,omitempty"`
		CheckedAt    int64  `json:"checked_at"`
	} `json:"identity,omitempty"`
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
	case "login", "login-status", "login-cancel", "login-code":
		return a.cmdAccountLogin(args[0], args[1:])
	case "add":
		return a.cmdAccountsAdd(args[1:])
	case "forget", "remove":
		return a.cmdAccountsForget(args[1:])
	case "list":
		return a.cmdProfiles(args[1:])
	default:
		return fail(1, "usage: sessions accounts [list | add <name> --tool claude|codex [--label TEXT] [--machine NAME] | forget <name> --tool claude|codex]")
	}
}

// cmdAccountsAdd registers the account and starts its bounded provider sign-in
// on the target machine. No agent conversation is created.
//
// --machine names the computer the account belongs to. A subscription is signed
// into per machine, so adding one on the mini from the laptop has to be a
// normal thing to ask for: the request goes through the same fleet relay every
// other --machine verb uses, and nothing local to this caller travels with it —
// least of all a working directory that need not exist over there.
func (a *app) cmdAccountsAdd(args []string) error {
	tool, _ := pluck(&args, "--tool")
	label, _ := pluck(&args, "--label")
	machine, hasMachine := pluck(&args, "--machine")
	if len(args) > 1 || (tool != "claude" && tool != "codex") {
		return fail(1, "usage: sessions accounts add [name] --tool claude|codex [--label TEXT] [--machine NAME]")
	}
	where := ""
	if hasMachine {
		alias, err := a.useAccountMachine(machine)
		if err != nil {
			return err
		}
		where = alias
	}
	name := ""
	if len(args) == 1 {
		name = args[0]
	}
	var created struct {
		Profile profileStatus `json:"profile"`
	}
	if err := a.postJSON("/api/profiles", map[string]string{"tool": tool, "name": name, "label": label}, &created, 2); err != nil {
		return err
	}
	var login map[string]any
	if err := a.postJSON("/api/account-logins", map[string]string{
		"tool": tool, "profile": created.Profile.Name,
	}, &login, 2); err != nil {
		return err
	}
	if a.wantJSON {
		answer := map[string]any{
			"account": created.Profile, "login": login,
		}
		if where != "" {
			answer["machine"] = where
		}
		return writeJSON(a.stdout, answer, true)
	}
	on := ""
	if where != "" {
		on = " on " + where
	}
	fmt.Fprintf(a.stdout, "account %s/%s registered at %s%s\n", tool, created.Profile.Name, created.Profile.Path, on)
	scope := ""
	if where != "" {
		scope = "--machine " + where + " "
	}
	fmt.Fprintf(a.stdout, "check sign-in with `sessions %saccounts login-status %v`\n", scope, login["id"])
	fmt.Fprintln(a.stdout, "check the account in the browser before confirming; Sessions will report the provider's account identity")
	return nil
}

// useAccountMachine points the rest of `accounts add` at an approved machine,
// and says what to do when the name is not one.
func (a *app) useAccountMachine(reference string) (string, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return "", fail(1, "--machine needs the name of an approved machine; `sessions machines` lists them")
	}
	alias, err := a.useMachine(reference)
	if err != nil {
		return "", fail(1, "%s", err)
	}
	return alias, nil
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
	rows := [][]string{{"TOOL", "NAME", "LABEL", "LOGIN-FILE", "SESSIONS", "LAST-USED", "PATH"}}
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
		// Presence of the file a provider writes when it signs in. Not proof
		// that the login still works, and a provider that keeps its credential
		// in the system keychain reports none.
		signedIn := "none"
		if profile.SignedIn {
			signedIn = "present"
		}
		rows = append(rows, []string{profile.Tool, profile.Name, label, signedIn, using, lastUsed, profile.Path})
	}
	if err := writePaddedRows(a.stdout, rows); err != nil {
		return err
	}
	_, err := fmt.Fprintln(a.stdout,
		"LOGIN-FILE is whether the file a provider writes at sign-in is present; Sessions does not open it, "+
			"so it is not proof that the login still works. Sessions never deletes profile credentials; "+
			"remove one manually only after reviewing its PATH above.")
	return err
}
