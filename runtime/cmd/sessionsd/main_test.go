package main

import "testing"

// The daemon must refuse every spelling of an all-interfaces bind, not just the
// four literals the original denylist contained. "0:0:0:0:0:0:0:0" and "0000::0"
// are valid IPv6 unspecified addresses that previously passed the check and
// bound every interface.
func TestIsWildcardHost(t *testing.T) {
	wildcard := []string{
		"0.0.0.0",
		"::",
		"::0",
		"*",
		"",
		"   ",
		"0:0:0:0:0:0:0:0",
		"0000::0",
		"[::]",
		"0.0.0.000",
	}
	for _, host := range wildcard {
		if !isWildcardHost(host) {
			t.Errorf("isWildcardHost(%q) = false, want true", host)
		}
	}

	specific := []string{
		"127.0.0.1",
		"::1",
		"[::1]",
		"192.168.1.20",
		"100.64.1.2",
		"localhost",
		"my-host.ts.net",
	}
	for _, host := range specific {
		if isWildcardHost(host) {
			t.Errorf("isWildcardHost(%q) = true, want false", host)
		}
	}
}
