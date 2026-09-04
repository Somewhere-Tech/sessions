package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"
	"github.com/somewhere-tech/sessions/runtime/internal/fleetendpoint"
)

type pairTicketResponse struct {
	Ticket    string                    `json:"ticket"`
	TicketID  string                    `json:"ticket_id"`
	ExpiresAt time.Time                 `json:"expires_at"`
	Link      string                    `json:"link"`
	Fallback  string                    `json:"fallback"`
	Endpoints []fleetendpoint.Candidate `json:"endpoints"`
}

func (a *app) cmdPair(args []string) error {
	name, hasName := pluck(&args, "--name")
	ttlRaw, hasTTL := pluck(&args, "--ttl")
	if hasName && strings.TrimSpace(name) == "" {
		return fail(1, "--name needs a non-empty device name")
	}
	if len(args) != 0 {
		return fail(1, "usage: sessions pair [--ttl 10m] [--name NAME]")
	}
	ttl := 10 * time.Minute
	var err error
	if hasTTL {
		ttl, err = time.ParseDuration(strings.TrimSpace(ttlRaw))
		if err != nil || ttl <= 0 || ttl > 10*time.Minute {
			return fail(1, "--ttl must be a positive duration no longer than 10m")
		}
	}

	var ticket pairTicketResponse
	if err := a.postJSON("/api/pair/ticket", map[string]string{
		"name": strings.TrimSpace(name), "ttl": ttl.String(),
	}, &ticket, 2); err != nil {
		return err
	}
	if ticket.Ticket == "" || ticket.TicketID == "" || ticket.Link == "" ||
		ticket.Fallback == "" || ticket.ExpiresAt.IsZero() || len(ticket.Endpoints) == 0 {
		return fail(2, "sessionsd returned an invalid pairing ticket; retry `sessions pair`, and check the daemon log if it still fails")
	}
	parsedEndpoints, parsedTicket, err := fleetendpoint.ParsePairingLink(ticket.Link)
	if err != nil || parsedTicket != ticket.Ticket || len(parsedEndpoints) != len(ticket.Endpoints) {
		return fail(2, "sessionsd returned an invalid pairing link; retry `sessions pair`, and check the daemon log if it still fails")
	}
	qrDataURL, err := pairingQRDataURL(ticket.Link)
	if err != nil {
		return fail(2, "encode pairing QR: %s", err)
	}
	if a.wantJSON {
		return writeJSON(a.stdout, struct {
			URL       string                    `json:"url"`
			Link      string                    `json:"link"`
			Fallback  string                    `json:"fallback"`
			QRDataURL string                    `json:"qr_data_url"`
			Endpoints []fleetendpoint.Candidate `json:"endpoints"`
			Ticket    string                    `json:"ticket"`
			TicketID  string                    `json:"ticket_id"`
			ExpiresAt time.Time                 `json:"expires_at"`
		}{ticket.Link, ticket.Link, ticket.Fallback, qrDataURL, ticket.Endpoints,
			ticket.Ticket, ticket.TicketID, ticket.ExpiresAt}, true)
	}

	if _, err := fmt.Fprintln(a.stdout, "\nPairing link ready."); err != nil {
		return err
	}
	if err := printQR(a.stdout, ticket.Link); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(a.stdout, "  App link: %s\n", ticket.Link); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(a.stdout, "  Web link: %s\n", ticket.Fallback); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(a.stdout, "  Expires: %s (%s; single use)\n", ticket.ExpiresAt.Local().Format(time.RFC3339), ttl); err != nil {
		return err
	}
	_, err = io.WriteString(a.stdout, "Revoke this unused link from Settings > Fleet.\n")
	return err
}

func pairingQRDataURL(link string) (string, error) {
	png, err := qrcode.Encode(link, qrcode.Medium, 384)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}
