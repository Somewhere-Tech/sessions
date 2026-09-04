package fleetendpoint

import (
	"reflect"
	"testing"
)

func TestEndpointPreferenceOrder(t *testing.T) {
	got := Ordered(
		"http://192.168.1.20:8787",
		"https://mini.example.ts.net",
		"http://100.100.20.30:8787",
	)
	want := []Candidate{
		{Endpoint: "http://192.168.1.20:8787", Transport: "lan"},
		{Endpoint: "https://mini.example.ts.net", Transport: "tailnet"},
		{Endpoint: "http://100.100.20.30:8787", Transport: "tailnet-ip"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ordered endpoints = %#v, want %#v", got, want)
	}
}
