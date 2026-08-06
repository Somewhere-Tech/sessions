package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	lanutil "github.com/somewhere-tech/sessions/runtime/internal/lan"
)

type lanState struct {
	Enabled bool    `json:"enabled"`
	URL     *string `json:"url"`
	Bonjour struct {
		Advertised bool   `json:"advertised"`
		Service    string `json:"service"`
		Error      string `json:"error,omitempty"`
	} `json:"bonjour"`
}

var primaryLANIPv4 = lanutil.PrimaryIPv4

func (a *app) cmdLan(args []string) error {
	if len(args) != 1 {
		return fail(1, "usage: sessions lan <enable|disable|status>")
	}
	switch args[0] {
	case "enable":
		return a.lanEnable()
	case "status":
		return a.lanStatus()
	case "disable":
		return a.lanDisable()
	default:
		return fail(1, "usage: sessions lan <enable|disable|status>")
	}
}

func (a *app) lanEnable() error {
	current, err := a.requestLAN(http.MethodPost, map[string]bool{"enabled": true}, "enable")
	if err != nil {
		return err
	}
	if !current.Enabled || current.URL == nil || *current.URL == "" {
		return fail(2, "sessionsd did not report an active LAN listener; retry `sessions lan enable`, and check the daemon log if it still fails")
	}
	if err := verifyLANEndpoint(*current.URL); err != nil {
		return failLANVerification(*current.URL, err)
	}
	return a.printLANConnection(current)
}

func (a *app) lanStatus() error {
	current, err := a.requestLAN(http.MethodGet, nil, "status")
	if err != nil {
		return err
	}
	if !current.Enabled || current.URL == nil || *current.URL == "" {
		if a.wantJSON {
			return writeJSON(a.stdout, struct {
				Enabled  bool    `json:"enabled"`
				Verified bool    `json:"verified"`
				URL      *string `json:"url"`
				Bonjour  any     `json:"bonjour"`
			}{false, false, nil, current.Bonjour}, false)
		}
		_, err := io.WriteString(a.stdout, "LAN access is disabled. Enable it with: sessions lan enable\n")
		return err
	}
	// A configured-but-unverified listener is the most common state this
	// command reports, and it used to leave --json callers with a bare error:
	// the URL, the enabled flag, and the Bonjour state were all thrown away
	// even though the daemon had just reported them. `remote status` answers
	// the analogous case with the configured state plus verified:false, and
	// this now matches it, so a client can still show and pair the endpoint.
	if !currentLANMatches(*current.URL) {
		if a.wantJSON {
			return a.writeLANStatus(current, "The current LAN IP no longer matches the listener; re-run `sessions lan enable` to bind sessionsd to this network.")
		}
		return fail(2, "LAN access is enabled at %s, but the current LAN IP no longer matches the listener. Re-run `sessions lan enable` to bind sessionsd to this network.", *current.URL)
	}
	if err := verifyLANEndpoint(*current.URL); err != nil {
		if a.wantJSON {
			return a.writeLANStatus(current, fmt.Sprintf("Health verification failed: %s. Make sure this Mac is still on that network and macOS Firewall allows sessionsd.", err))
		}
		return failLANVerification(*current.URL, err)
	}
	return a.printLANConnection(current)
}

// writeLANStatus reports a configured LAN listener that did not verify.
func (a *app) writeLANStatus(current lanState, verificationError string) error {
	endpoint := *current.URL
	return writeJSON(a.stdout, struct {
		Enabled           bool    `json:"enabled"`
		Verified          bool    `json:"verified"`
		URL               *string `json:"url"`
		Bonjour           any     `json:"bonjour"`
		VerificationError string  `json:"verificationError"`
	}{true, false, &endpoint, current.Bonjour, verificationError}, false)
}

func (a *app) lanDisable() error {
	before, err := a.requestLAN(http.MethodGet, nil, "disable")
	if err != nil {
		return err
	}
	if _, err := a.requestLAN(http.MethodPost, map[string]bool{"enabled": false}, "disable"); err != nil {
		return err
	}
	after, err := a.requestLAN(http.MethodGet, nil, "disable")
	if err != nil {
		return err
	}
	if after.Enabled || after.URL != nil {
		return fail(2, "sessionsd still reports a LAN listener; LAN access was not disabled. Retry `sessions lan disable` and check the daemon log.")
	}
	if a.wantJSON {
		return writeJSON(a.stdout, struct {
			Enabled bool `json:"enabled"`
			Changed bool `json:"changed"`
		}{false, before.Enabled}, false)
	}
	message := "LAN access disabled.\n"
	if !before.Enabled {
		message = "LAN access is already disabled.\n"
	}
	_, err = io.WriteString(a.stdout, message)
	return err
}

func (a *app) requestLAN(method string, body any, action string) (lanState, error) {
	response, err := a.api.request(context.Background(), method, "/api/lan", body, 5*time.Second)
	if err != nil {
		endpoint := "sessionsd"
		if target, targetErr := a.api.target("/"); targetErr == nil {
			endpoint = target.Scheme + "://" + target.Host
		}
		return lanState{}, fail(2, "cannot reach sessionsd at %s: %s. Start it first with `sessions install`, then retry `sessions lan %s`.", endpoint, err, action)
	}
	if response.status >= 400 {
		var payload struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(response.body, &payload) == nil && payload.Error != "" {
			return lanState{}, fail(2, "%s", payload.Error)
		}
		return lanState{}, fail(2, "/api/lan returned HTTP %d: %s", response.status, prefixBytes(response.body, 200))
	}
	var current lanState
	if err := json.Unmarshal(response.body, &current); err != nil {
		return lanState{}, fail(2, "sessionsd returned an invalid LAN status: %s", err)
	}
	return current, nil
}

func verifyLANEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() == "" {
		return fmt.Errorf("invalid LAN URL %q", endpoint)
	}
	healthURL := *parsed
	healthURL.Path = "/api/health"
	healthURL.RawPath = ""
	healthURL.RawQuery = ""
	healthURL.Fragment = ""
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	client := &http.Client{
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
		Timeout:   5 * time.Second,
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned HTTP %d", healthURL.String(), response.StatusCode)
	}
	return nil
}

func currentLANMatches(endpoint string) bool {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	listenerIP := net.ParseIP(parsed.Hostname())
	currentIP, err := primaryLANIPv4()
	return err == nil && listenerIP != nil && currentIP.Equal(listenerIP)
}

func failLANVerification(endpoint string, err error) error {
	return fail(2, "LAN access is configured at %s, but health verification failed: %s. Make sure this Mac is still on that network and macOS Firewall allows sessionsd, then retry `sessions lan enable`.", endpoint, err)
}

func (a *app) printLANConnection(current lanState) error {
	endpoint := *current.URL
	if a.wantJSON {
		url := endpoint
		return writeJSON(a.stdout, struct {
			Enabled  bool    `json:"enabled"`
			Verified bool    `json:"verified"`
			URL      *string `json:"url"`
			Bonjour  any     `json:"bonjour"`
		}{true, true, &url, current.Bonjour}, false)
	}
	io.WriteString(a.stdout, "\nLAN access verified (HTTP 200).\n")
	fmt.Fprintf(a.stdout, "  URL: %s\n", endpoint)
	if current.Bonjour.Advertised {
		fmt.Fprintf(a.stdout, "  Discovery: Bonjour %s advertised\n", current.Bonjour.Service)
	} else if current.Bonjour.Error != "" {
		fmt.Fprintf(a.stdout, "  Discovery: unavailable (%s)\n", current.Bonjour.Error)
	}
	io.WriteString(a.stdout, "Nearby Sessions apps and `sessions machines discover` can request access.\n")
	io.WriteString(a.stdout, "For a device on this network without discovery, run `sessions pair` and scan its one-time QR.\n")
	_, err := io.WriteString(a.stdout, "LAN access works only on this network. From anywhere: sessions remote enable\n")
	return err
}
