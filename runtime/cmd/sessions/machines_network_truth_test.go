package main

import (
	"bytes"
	"runtime"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/localnetwork"
)

// An empty browse means no machine answered. It is not a reading of the macOS
// Local Network switch, which has no supported API: an earlier build failed
// here with a flat denial and sent people to a settings pane that was already
// correct.
func TestDiscoverWithNoPeersReportsWhatWasObserved(t *testing.T) {
	var output bytes.Buffer
	application := &app{stdout: &output}
	if err := application.writeDiscoveredMachines(nil); err != nil {
		t.Fatalf("write discovered machines: %v", err)
	}
	got := output.String()
	if !strings.Contains(got, "No nearby Sessions machines found") {
		t.Fatalf("discover output = %q", got)
	}
	if strings.Contains(got, "has not allowed") {
		t.Fatalf("discover output asserted a macOS denial: %q", got)
	}
	if runtime.GOOS == "darwin" && !strings.Contains(got, localnetwork.PossibleCause) {
		t.Fatalf("discover output = %q, want the permission offered as a possible cause", got)
	}
}

// doctor reports the daemon's observation. Only an older daemon still sends a
// denial, and then its own sentence is the one to print.
func TestDoctorLocalNetworkReportsObservationsNotVerdicts(t *testing.T) {
	tests := map[string]struct {
		permission map[string]any
		want       string
		absent     string
	}{
		"unproven": {
			map[string]any{"status": "not-yet-asked"},
			"local network: not confirmed", "denied",
		},
		// A stored success is history. Reporting it as a present fact is what
		// sent people to a settings pane after the condition had already passed.
		"granted": {
			map[string]any{"status": "granted"},
			"nearby access worked at the host's last observation; this is not a live check", "denied",
		},
		// An older host's inference is that host's claim, not this CLI's reading
		// of a macOS switch it cannot read.
		"legacy denial from an older daemon": {
			map[string]any{"status": "denied", "message": "older host sentence"},
			"local network: the host (older version) reported denied — older host sentence", "System Settings",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			writeDoctorLocalNetwork(&output, map[string]any{"permission": test.permission})
			got := output.String()
			if !strings.Contains(got, test.want) {
				t.Fatalf("doctor local network = %q, want %q", got, test.want)
			}
			if strings.Contains(got, test.absent) {
				t.Fatalf("doctor local network = %q, want it to omit %q", got, test.absent)
			}
		})
	}
}
