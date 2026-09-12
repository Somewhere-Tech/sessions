package background

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

// From the Mini, 11 September: 100% CPU for minutes after health said ready,
// and nothing in the log naming the work. A pass that ran has to be countable
// even when it was too short to deserve a line of its own.
func TestEveryPassIsCountedAndOnlyLongOnesAreLogged(t *testing.T) {
	var written bytes.Buffer
	restore := captureLog(t, &written)
	defer restore()

	before := Totals()["fixture-quick"]
	for range 5 {
		Start("fixture-quick").Done()
	}
	after := Totals()["fixture-quick"]
	if after.Runs-before.Runs != 5 {
		t.Fatalf("five passes were counted as %d", after.Runs-before.Runs)
	}
	if strings.Contains(written.String(), "fixture-quick") {
		t.Fatalf("a sub-second pass wrote to the log: %q", written.String())
	}

	// A pass over the floor says what it cost, under its own name.
	slow := Start("fixture-slow")
	slow.started = slow.started.Add(-2 * time.Second)
	slow.Done()
	line := written.String()
	if !strings.Contains(line, "[background] fixture-slow took 2.0s") {
		t.Fatalf("a two-second pass logged %q", line)
	}
	if !strings.Contains(line, "process cpu") {
		t.Fatalf("the line does not say what it cost the machine: %q", line)
	}
}

// The report is what an operator reads while a burst is happening, so it has to
// carry every pass with its runs and its cost.
func TestTheReportCarriesRunsAndCost(t *testing.T) {
	pass := Start("fixture-reported")
	pass.started = pass.started.Add(-1500 * time.Millisecond)
	pass.Done()

	entry, ok := Report()["fixture-reported"].(map[string]any)
	if !ok {
		t.Fatalf("the report has no entry for a pass that ran: %#v", Report())
	}
	if runs, ok := entry["runs"].(int64); !ok || runs < 1 {
		t.Fatalf("runs = %#v", entry["runs"])
	}
	if took, ok := entry["ms"].(int64); !ok || took < 1_500 {
		t.Fatalf("ms = %#v, want at least the 1500 the pass took", entry["ms"])
	}
	if _, ok := entry["cpu_ms"].(int64); !ok {
		t.Fatalf("cpu_ms = %#v", entry["cpu_ms"])
	}
}

// A caller that decided not to measure this run must not have to branch around
// every call it makes.
func TestANilPassIsSafe(t *testing.T) {
	var pass *Pass
	pass.Done()
}

func captureLog(t *testing.T, into *bytes.Buffer) func() {
	t.Helper()
	previous := log.Writer()
	flags := log.Flags()
	log.SetOutput(into)
	log.SetFlags(0)
	return func() {
		log.SetOutput(previous)
		log.SetFlags(flags)
	}
}
