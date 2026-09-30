package mirror

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// A mirror that gave its emulator back must come back the same mirror.
//
// Every read the daemon performs — the session snapshot, `sessions snap`, the
// idle classifier's terminal tail, the attach replay and the reflowed view —
// reads one of the four methods compared here. Two mirrors are fed the same
// recorded output; one of them hibernates and wakes between every step. If any
// of the four ever differ, hibernation is not free and this test says where.

type mirrorStep struct {
	what   string
	write  string
	resize [2]int
}

func recordedOutput() []mirrorStep {
	steps := []mirrorStep{
		{what: "plain lines", write: "hello world\r\nsecond line\r\n"},
		{what: "colours and bold", write: "\x1b[1;31mred bold\x1b[0m plain \x1b[44mon blue\x1b[0m\r\n"},
		{what: "wide runes", write: "日本語のテキスト and émojis 🚀🚀\r\n"},
		{what: "combining marks", write: "é à ö done\r\n"},
		{what: "soft wrap past the edge", write: strings.Repeat("w", 90) + "\r\n"},
		{what: "cursor moves and erase", write: "\x1b[3;2Hplaced\x1b[K\r\n"},
		{what: "carriage return overwrite", write: "aaaaaaa\rbbb\r\n"},
		{what: "hyperlink", write: "\x1b]8;;https://example.invalid\x1b\\link\x1b]8;;\x1b\\\r\n"},
		{what: "resize narrower", resize: [2]int{40, 10}},
		{what: "text after resize", write: "after the narrower resize\r\n"},
		{what: "resize wider", resize: [2]int{100, 12}},
		{what: "text after widening", write: "after the wider resize " + strings.Repeat("x", 60) + "\r\n"},
	}
	// Enough lines to scroll the viewport and fill history, which is what
	// hibernation has to carry across in the serialized stream.
	var history strings.Builder
	for line := 0; line < 120; line++ {
		fmt.Fprintf(&history, "history line %03d\r\n", line)
	}
	steps = append(steps,
		mirrorStep{what: "scrollback overflow", write: history.String()},
		mirrorStep{what: "styled tail after scrolling", write: "\x1b[32mtail\x1b[0m\r\n"},
		mirrorStep{what: "erase display", write: "\x1b[2J\x1b[Hcleared and repainted\r\n"},
	)
	return steps
}

func applyStep(t *testing.T, m *Mirror, step mirrorStep) {
	t.Helper()
	if step.resize != [2]int{} {
		if err := m.Resize(step.resize[0], step.resize[1]); err != nil {
			t.Fatalf("Resize(%d, %d): %v", step.resize[0], step.resize[1], err)
		}
		return
	}
	writeString(t, m, step.write)
}

func compareReads(t *testing.T, what string, kept, cycled *Mirror) {
	t.Helper()
	for _, read := range []struct {
		name string
		of   func(*Mirror) string
	}{
		{"Snapshot", (*Mirror).Snapshot},
		{"SerializeANSI", (*Mirror).SerializeANSI},
		{"SerializeANSIWithScrollback", (*Mirror).SerializeANSIWithScrollback},
		{"ReflowTo(80)", func(m *Mirror) string { return m.ReflowTo(80) }},
	} {
		want, got := read.of(kept), read.of(cycled)
		if want != got {
			t.Errorf("after %q, %s differs after hibernation\n kept:   %q\n cycled: %q",
				what, read.name, want, got)
		}
	}
}

func TestHibernationKeepsEveryReadByteIdentical(t *testing.T) {
	kept := newTestMirror(t, 80, 24)
	cycled := newTestMirror(t, 80, 24)
	for _, step := range recordedOutput() {
		applyStep(t, kept, step)
		applyStep(t, cycled, step)
		if !cycled.Hibernate() {
			t.Fatalf("after %q the mirror refused to hibernate", step.what)
		}
		if !cycled.Hibernating() {
			t.Fatalf("after %q the mirror reports it still holds an emulator", step.what)
		}
		compareReads(t, step.what, kept, cycled)
		if cycled.Hibernating() {
			t.Fatalf("after %q a read left the mirror hibernated", step.what)
		}
	}
}

// Waking has to be invisible to what comes next, not only to what came before:
// output written after a wake must land exactly where it would have.
func TestOutputAfterWakingLandsWhereItWould(t *testing.T) {
	kept := newTestMirror(t, 40, 8)
	cycled := newTestMirror(t, 40, 8)
	for _, step := range recordedOutput() {
		applyStep(t, kept, step)
		applyStep(t, cycled, step)
		cycled.Hibernate()
		// No read between hibernating and writing: the write itself wakes it.
		applyStep(t, kept, mirrorStep{write: "\x1b[33mfollow-up\x1b[0m line\r\n"})
		applyStep(t, cycled, mirrorStep{write: "\x1b[33mfollow-up\x1b[0m line\r\n"})
		compareReads(t, step.what+" then a write", kept, cycled)
	}
}

// A screen state the serialized stream does not carry keeps its emulator.
func TestHibernationRefusesWhatItCannotCarry(t *testing.T) {
	alternate := newTestMirror(t, 20, 5)
	writeString(t, alternate, "\x1b[?1049hinside the alternate screen")
	if alternate.Hibernate() {
		t.Fatal("a mirror showing an alternate screen hibernated")
	}
	region := newTestMirror(t, 20, 5)
	writeString(t, region, "\x1b[2;4rinside a scroll region")
	if region.Hibernate() {
		t.Fatal("a mirror inside a scroll region hibernated")
	}
	partial := newTestMirror(t, 20, 5)
	writeString(t, partial, "text\x1b[1;3")
	if partial.Hibernate() {
		t.Fatal("a mirror holding a half-parsed sequence hibernated")
	}
}

// Closing a hibernated mirror must not wake it, and must stay idempotent.
func TestClosingAHibernatedMirrorIsQuiet(t *testing.T) {
	m, err := NewSize(20, 5)
	if err != nil {
		t.Fatal(err)
	}
	writeString(t, m, "some output\r\n")
	if !m.Hibernate() {
		t.Fatal("mirror refused to hibernate")
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if m.Hibernating() {
		t.Fatal("a closed mirror reports itself hibernated")
	}
}

// Hibernation is driven by idleness, not by counting viewers: a client that is
// streaming reads the mirror once when it attaches, and a runner that is
// producing output wakes it on the next write.
func TestHibernateIfIdleWaitsForQuiet(t *testing.T) {
	m := newTestMirror(t, 20, 5)
	if m.HibernateIfIdle(time.Hour) {
		t.Fatal("a mirror touched a moment ago hibernated")
	}
	if !m.HibernateIfIdle(0) {
		t.Fatal("an idle mirror kept its emulator")
	}
	if !m.HibernateIfIdle(time.Hour) {
		t.Fatal("an already hibernated mirror reported otherwise")
	}
	writeString(t, m, "back awake\r\n")
	if m.Hibernating() {
		t.Fatal("a write left the mirror hibernated")
	}
	if m.HibernateIfIdle(time.Hour) {
		t.Fatal("a mirror written to a moment ago hibernated")
	}
	if got := m.Snapshot(); got != "back awake" {
		t.Fatalf("Snapshot() = %q, want %q", got, "back awake")
	}
}

// A mirror that never received a byte is the cheapest thing to give back, and
// the most common one on a machine holding many sessions at once.
func TestNeverWrittenMirrorHibernatesToNothing(t *testing.T) {
	m := newTestMirror(t, 300, 50)
	if !m.HibernateIfIdle(0) {
		t.Fatal("an untouched mirror kept its emulator")
	}
	if got := m.Snapshot(); got != "" {
		t.Fatalf("Snapshot() = %q, want empty", got)
	}
	if m.Hibernating() {
		t.Fatal("reading left the mirror hibernated")
	}
}

// History is bounded by what it costs, not only by how many lines it is. Five
// thousand full-width lines are two hundred megabytes of cells; the same five
// thousand shell prompts are a few. Only one of those is worth keeping whole.
func TestScrollbackIsBoundedByBytes(t *testing.T) {
	m := newTestMirror(t, 300, 5)
	wide := strings.Repeat("W", 300)
	for line := 0; line < 400; line++ {
		writeString(t, m, wide+"\r\n")
	}
	scrollback := m.term.Scrollback()
	retained := 0
	for _, line := range scrollback.Lines() {
		retained += len(line) * scrollbackCellBytes
	}
	if retained > DefaultScrollbackBudgetBytes {
		t.Fatalf("retained history is %d bytes, want at most %d", retained, DefaultScrollbackBudgetBytes)
	}
	if scrollback.Len() == 0 {
		t.Fatal("the budget dropped every line of history")
	}
	// The newest history is what a person scrolls back to first, so that is
	// what a budget keeps.
	newest := scrollback.Lines()[scrollback.Len()-1]
	if got := strings.TrimRight(newest.String(), " "); got != wide {
		t.Fatalf("newest retained line = %q, want the last line written", got)
	}
	// The same history, narrow, stays whole: a budget in bytes does not punish
	// a session for having scrolled.
	narrow := newTestMirror(t, 300, 5)
	for line := 0; line < 400; line++ {
		writeString(t, narrow, fmt.Sprintf("line %d\r\n", line))
	}
	if got := narrow.term.Scrollback().Len(); got != 396 {
		t.Fatalf("narrow history retained %d lines, want all 396 scrolled ones", got)
	}
}
