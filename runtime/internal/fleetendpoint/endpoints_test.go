package fleetendpoint

import (
	"reflect"
	"strings"
	"testing"
)

func TestEndpointPreferenceOrder(t *testing.T) {
	got := OrderedWithRelay(
		"http://192.168.1.20:8787",
		"https://mini.example.ts.net",
		"http://100.100.20.30:8787",
		"https://relay.example/m/mini",
	)
	want := []Candidate{
		{Endpoint: "http://192.168.1.20:8787", Transport: "lan"},
		{Endpoint: "https://mini.example.ts.net", Transport: "tailnet"},
		{Endpoint: "http://100.100.20.30:8787", Transport: "tailnet-ip"},
		{Endpoint: "https://relay.example/m/mini", Transport: "relay"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ordered endpoints = %#v, want %#v", got, want)
	}
}

func TestPairingLinkRoundTripKeepsEndpointOrder(t *testing.T) {
	candidates := Ordered(
		"http://192.168.1.20:8787",
		"https://mini.example.ts.net",
		"http://100.100.20.30:8787",
	)
	link, err := PairingLink(candidates, "ticket.secret")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link, "sessions://pair?") {
		t.Fatalf("pairing link = %q", link)
	}
	endpoints, ticket, err := ParsePairingLink(link)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"http://192.168.1.20:8787",
		"https://mini.example.ts.net",
		"http://100.100.20.30:8787",
	}
	if !reflect.DeepEqual(endpoints, want) || ticket != "ticket.secret" {
		t.Fatalf("parsed link = %#v, %q; want %#v, ticket.secret", endpoints, ticket, want)
	}
}

func TestPlainAndLegacyPairingLinksRemainReadable(t *testing.T) {
	for _, test := range []struct {
		link, endpoint, ticket string
	}{
		{"https://mini.example.ts.net/pair/ticket.secret", "https://mini.example.ts.net", "ticket.secret"},
		{"http://192.168.1.20:8787/#pair=ticket.secret", "http://192.168.1.20:8787", "ticket.secret"},
	} {
		endpoints, ticket, err := ParsePairingLink(test.link)
		if err != nil || !reflect.DeepEqual(endpoints, []string{test.endpoint}) || ticket != test.ticket {
			t.Fatalf("ParsePairingLink(%q) = %#v, %q, %v", test.link, endpoints, ticket, err)
		}
	}
}
