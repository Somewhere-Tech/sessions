package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// A runner restarted by launchd after a crash re-runs the provider. Handing it
// back a --session-id it has already used makes the provider refuse, and the
// conversation is lost rather than resumed. The joined spelling was the case
// that got missed.
func TestIndependentRespawnConvertsBothSpellings(t *testing.T) {
	const uuid = "e2f40520-dcf2-464d-a88c-dc067f3ff904"
	for _, spelling := range [][]string{
		{"--session-id", uuid, "--settings", "{}"},
		{"--session-id=" + uuid, "--settings", "{}"},
	} {
		events := filepath.Join(t.TempDir(), "events")
		if err := os.WriteFile(events, []byte("prior conversation\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, _ := respawnArgs(config{cmd: "claude", args: spelling, kind: string(state.ToolClaude)}, events)
		joined := strings.Join(got, " ")
		if strings.Contains(joined, "--session-id") {
			t.Fatalf("%v respawned still claiming a fresh session: %q", spelling, joined)
		}
		if !strings.Contains(joined, "--resume "+uuid) && !strings.Contains(joined, "--resume="+uuid) {
			t.Fatalf("%v respawned without resuming the conversation: %q", spelling, joined)
		}
	}
}
