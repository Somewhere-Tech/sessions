package fleetendpoint

import (
	"errors"
	"net/url"
	"strings"
)

type Candidate struct {
	Endpoint  string `json:"endpoint"`
	Transport string `json:"transport"`
}

// Ordered is the one fleet transport policy: nearby LAN first, MagicDNS HTTPS
// second, and the Tailscale CGNAT address last when peer DNS is unavailable.
func Ordered(lan, tailnet, tailnetIP string) []Candidate {
	values := []Candidate{
		{Endpoint: lan, Transport: "lan"},
		{Endpoint: tailnet, Transport: "tailnet"},
		{Endpoint: tailnetIP, Transport: "tailnet-ip"},
	}
	seen := make(map[string]bool, len(values))
	result := make([]Candidate, 0, len(values))
	for _, candidate := range values {
		candidate.Endpoint = strings.TrimSuffix(strings.TrimSpace(candidate.Endpoint), "/")
		if candidate.Endpoint == "" || seen[candidate.Endpoint] {
			continue
		}
		seen[candidate.Endpoint] = true
		result = append(result, candidate)
	}
	return result
}

// PairingLink carries every advertised route to one machine. Repeated host
// parameters preserve the daemon's transport order without teaching the URL
// format about transport names; each client validates and classifies the
// origins before dialing them.
func PairingLink(candidates []Candidate, ticket string) (string, error) {
	if len(candidates) == 0 || strings.TrimSpace(ticket) == "" {
		return "", errors.New("pairing requires an endpoint and ticket")
	}
	query := url.Values{}
	for _, candidate := range candidates {
		if endpoint := strings.TrimSpace(candidate.Endpoint); endpoint != "" {
			query.Add("host", strings.TrimSuffix(endpoint, "/"))
		}
	}
	if len(query["host"]) == 0 {
		return "", errors.New("pairing requires an endpoint")
	}
	query.Set("t", strings.TrimSpace(ticket))
	return "sessions://pair?" + query.Encode(), nil
}

// ParsePairingLink accepts the current multi-endpoint application link and
// the plain browser fallback. The legacy fragment form remains readable so an
// unexpired link minted by an older daemon does not become unusable mid-flight.
func ParsePairingLink(value string) ([]string, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" {
		return nil, "", errors.New("paste the full pairing link")
	}
	if parsed.Scheme == "sessions" && parsed.Host == "pair" {
		return parseApplicationPairingLink(parsed)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, "", errors.New("pairing link must use sessions, HTTPS, or trusted-LAN HTTP")
	}
	if ticket, ok := strings.CutPrefix(parsed.EscapedPath(), "/pair/"); ok && ticket != "" {
		decoded, decodeErr := url.PathUnescape(ticket)
		if decodeErr != nil || decoded == "" || strings.Contains(decoded, "/") {
			return nil, "", errors.New("pairing link has an invalid ticket")
		}
		parsed.Path, parsed.RawPath, parsed.RawQuery, parsed.Fragment = "", "", "", ""
		return []string{strings.TrimSuffix(parsed.String(), "/")}, decoded, nil
	}
	legacy := new(url.URL)
	*legacy = *parsed
	fragment, err := url.ParseQuery(parsed.Fragment)
	if err != nil || len(fragment["pair"]) != 1 || len(fragment) != 1 {
		return nil, "", errors.New("pairing link is missing its one-time ticket")
	}
	legacy.Fragment = ""
	legacy.Path, legacy.RawPath, legacy.RawQuery = "", "", ""
	return []string{strings.TrimSuffix(legacy.String(), "/")}, fragment.Get("pair"), nil
}

func parseApplicationPairingLink(parsed *url.URL) ([]string, string, error) {
	if parsed.Path != "" || parsed.Fragment != "" || parsed.User != nil {
		return nil, "", errors.New("pairing link has unexpected fields")
	}
	query := parsed.Query()
	hosts, tickets := query["host"], query["t"]
	if len(query) != 2 || len(hosts) == 0 || len(tickets) != 1 || strings.TrimSpace(tickets[0]) == "" {
		return nil, "", errors.New("pairing link has invalid endpoints or ticket")
	}
	endpoints := make([]string, 0, len(hosts))
	seen := make(map[string]bool, len(hosts))
	for _, host := range hosts {
		host = strings.TrimSuffix(strings.TrimSpace(host), "/")
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		endpoints = append(endpoints, host)
	}
	if len(endpoints) == 0 {
		return nil, "", errors.New("pairing link has no machine endpoint")
	}
	return endpoints, strings.TrimSpace(tickets[0]), nil
}
