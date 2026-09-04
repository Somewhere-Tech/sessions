package fleetendpoint

import "strings"

type Candidate struct {
	Endpoint  string
	Transport string
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
