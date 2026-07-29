package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type pairTicketResponse struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (a *app) cmdPair(args []string) error {
	name, hasName := pluck(&args, "--name")
	if hasName && strings.TrimSpace(name) == "" {
		return fail(1, "--name needs a non-empty device name")
	}
	if len(args) != 0 {
		return fail(1, "usage: sessions pair [--name NAME]")
	}

	endpoint, err := a.verifiedPairLANEndpoint()
	if err != nil {
		return fail(2, "LAN pairing is unavailable: %s. Enable it with `sessions lan enable`, then retry. Tailscale devices request access directly in Sessions.app and do not use a QR code.", err)
	}

	var ticket pairTicketResponse
	if err := a.postJSON("/api/pair/ticket", map[string]string{"name": strings.TrimSpace(name)}, &ticket, 2); err != nil {
		return err
	}
	if ticket.Ticket == "" || ticket.ExpiresAt.IsZero() {
		return fail(2, "sessionsd returned an invalid pairing ticket; retry `sessions pair`, and check the daemon log if it still fails")
	}

	pairURL, err := pairingURL(endpoint, ticket.Ticket)
	if err != nil {
		return fail(2, "sessionsd reported an invalid pairing endpoint %q: %s", endpoint, err)
	}
	if a.wantJSON {
		return writeJSON(a.stdout, struct {
			URL       string    `json:"url"`
			Endpoint  string    `json:"endpoint"`
			Ticket    string    `json:"ticket"`
			ExpiresAt time.Time `json:"expires_at"`
		}{pairURL, endpoint, ticket.Ticket, ticket.ExpiresAt}, true)
	}

	if _, err := fmt.Fprintln(a.stdout, "\nPairing link ready."); err != nil {
		return err
	}
	if err := printQR(a.stdout, pairURL); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(a.stdout, "  URL: %s\n", pairURL); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(a.stdout, "  Expires: %s (five minutes; single use)\n", ticket.ExpiresAt.Local().Format(time.RFC3339)); err != nil {
		return err
	}
	_, err = io.WriteString(a.stdout, "already have the page open? Paste the ticket in Settings → Pair.\n")
	return err
}

func (a *app) verifiedPairLANEndpoint() (string, error) {
	current, err := a.requestLAN(http.MethodGet, nil, "status")
	if err != nil {
		return "", err
	}
	if !current.Enabled || current.URL == nil || *current.URL == "" {
		return "", errors.New("LAN access is disabled")
	}
	if !currentLANMatches(*current.URL) {
		return "", errors.New("LAN listener no longer matches the current network")
	}
	if err := verifyLANEndpoint(*current.URL); err != nil {
		return "", err
	}
	return *current.URL, nil
}

func pairingURL(endpoint, ticket string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("endpoint must be an absolute URL")
	}
	parsed.Path = "/"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = "pair=" + url.QueryEscape(ticket)
	return parsed.String(), nil
}
