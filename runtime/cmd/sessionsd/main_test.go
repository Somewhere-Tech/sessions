package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestHandleDaemonArgsDoesNotStartForHelpOrVersion(t *testing.T) {
	for _, test := range []struct {
		arguments []string
		want      string
	}{
		{arguments: []string{"--help"}, want: "Usage: sessionsd"},
		{arguments: []string{"--version"}, want: version},
	} {
		var output bytes.Buffer
		handled, err := handleDaemonArgs(test.arguments, &output)
		if err != nil || !handled || !strings.Contains(output.String(), test.want) {
			t.Fatalf("handleDaemonArgs(%q) = handled=%v output=%q err=%v", test.arguments, handled, output.String(), err)
		}
	}
}

func TestHandleDaemonArgsAcceptsServeAndRejectsTypos(t *testing.T) {
	if handled, err := handleDaemonArgs([]string{"--serve"}, &bytes.Buffer{}); err != nil || handled {
		t.Fatalf("--serve = handled=%v err=%v", handled, err)
	}
	if _, err := handleDaemonArgs([]string{"--hepl"}, &bytes.Buffer{}); err == nil {
		t.Fatal("unknown argument was accepted")
	}
}

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
