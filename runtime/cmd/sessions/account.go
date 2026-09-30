package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

type fleetAccountUser struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
}

type fleetAccountStatus struct {
	SignedIn              bool              `json:"signed_in"`
	User                  *fleetAccountUser `json:"user,omitempty"`
	MachinePublicKey      string            `json:"machine_public_key,omitempty"`
	LastRegistrationAt    string            `json:"last_registration_at,omitempty"`
	LastRegistrationError string            `json:"last_registration_error,omitempty"`
	LastHeartbeatAt       string            `json:"last_heartbeat_at,omitempty"`
}

func (a *app) cmdAccount(args []string) error {
	if len(args) == 0 {
		return fail(1, "usage: sessions account <login|logout|status|key>")
	}
	switch args[0] {
	case "login":
		return a.accountLogin(args[1:])
	case "logout":
		return a.accountLogout(args[1:])
	case "status":
		return a.accountStatus(args[1:])
	case "key":
		return a.accountKey(args[1:])
	default:
		return fail(1, "unknown account action %q; use login, logout, status, or key", args[0])
	}
}

func (a *app) accountLogin(args []string) error {
	email, _ := pluck(&args, "--email")
	code, _ := pluck(&args, "--code")
	if len(args) != 0 {
		return fail(1, "usage: sessions account login [--email EMAIL] [--code CODE]")
	}
	reader := bufio.NewReader(a.stdin)
	var err error
	if strings.TrimSpace(email) == "" {
		email, err = promptAccountValue(reader, a.stderr, "Somewhere email: ")
		if err != nil {
			return err
		}
	}
	if err := a.postJSON("/api/account/magic-link", map[string]string{"email": email}, nil, 2); err != nil {
		return err
	}
	if strings.TrimSpace(code) == "" {
		fmt.Fprintln(a.stderr, "Somewhere sent a single-use sign-in code or link to "+strings.TrimSpace(email)+".")
		code, err = promptAccountValue(reader, a.stderr, "Code: ")
		if err != nil {
			return err
		}
	}
	var status fleetAccountStatus
	if err := a.postJSON("/api/account/verify", map[string]string{"token": code}, &status, 2); err != nil {
		return err
	}
	return a.writeAccountStatus(status, "Signed in")
}

func promptAccountValue(reader *bufio.Reader, writer io.Writer, prompt string) (string, error) {
	if _, err := io.WriteString(writer, prompt); err != nil {
		return "", err
	}
	value, err := reader.ReadString('\n')
	if err != nil && len(value) == 0 {
		return "", fail(1, "could not read sign-in input: %s", err)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fail(1, "sign-in input cannot be empty")
	}
	return value, nil
}

func (a *app) accountLogout(args []string) error {
	if len(args) != 0 {
		return fail(1, "usage: sessions account logout")
	}
	var result map[string]any
	if err := a.postJSON("/api/account/logout", nil, &result, 2); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, result, true)
	}
	_, err := fmt.Fprintln(a.stdout, "Signed out of Somewhere. Local and tailnet Sessions access is unchanged.")
	return err
}

func (a *app) accountStatus(args []string) error {
	if len(args) != 0 {
		return fail(1, "usage: sessions account status")
	}
	var status fleetAccountStatus
	if err := a.getJSON("/api/account", &status); err != nil {
		return err
	}
	return a.writeAccountStatus(status, "Somewhere account")
}

func (a *app) writeAccountStatus(status fleetAccountStatus, heading string) error {
	if a.wantJSON {
		return writeJSON(a.stdout, status, true)
	}
	if !status.SignedIn {
		_, err := fmt.Fprintln(a.stdout, heading+": signed out\nSessions works on this network and tailnet without an account.")
		return err
	}
	identity := "Signed in"
	if status.User != nil && status.User.Email != "" {
		identity = status.User.Email
	}
	if status.User != nil && status.User.DisplayName != "" {
		identity = status.User.DisplayName + " <" + status.User.Email + ">"
	}
	fmt.Fprintf(a.stdout, "%s: %s\n", heading, identity)
	if status.LastRegistrationAt != "" {
		fmt.Fprintf(a.stdout, "Machine registered: %s\n", status.LastRegistrationAt)
	}
	if status.LastRegistrationError != "" {
		fmt.Fprintf(a.stdout, "Machine registration pending: %s\n", status.LastRegistrationError)
	}
	return nil
}

func (a *app) accountKey(args []string) error {
	if len(args) != 0 {
		return fail(1, "usage: sessions account key")
	}
	var key struct {
		PublicKey string `json:"public_key"`
	}
	if err := a.getJSON("/api/account/key", &key); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, key, true)
	}
	_, err := fmt.Fprintln(a.stdout, key.PublicKey)
	return err
}
