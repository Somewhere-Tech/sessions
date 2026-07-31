package mirror

import (
	"strings"
	"testing"
	"time"
)

func newTestMirror(t *testing.T, cols, rows int) *Mirror {
	t.Helper()
	m, err := NewSize(cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return m
}

func writeString(t *testing.T, m *Mirror, s string) {
	t.Helper()
	if n, err := m.Write([]byte(s)); err != nil || n != len(s) {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(s))
	}
}

func TestNewSizeRejectsInvalidDimensions(t *testing.T) {
	for _, size := range [][2]int{{0, 1}, {1, 0}, {-1, 2}, {2, -1}} {
		if _, err := NewSize(size[0], size[1]); err == nil {
			t.Errorf("NewSize(%d, %d) unexpectedly succeeded", size[0], size[1])
		}
	}
}

func TestSnapshotCursorEraseAndWrap(t *testing.T) {
	m := newTestMirror(t, 5, 3)
	writeString(t, m, "abc\r\n123\x1b[1;2HZ\x1b[2;2H\x1b[K")
	if got, want := m.Snapshot(), "aZc\n1"; got != want {
		t.Fatalf("Snapshot() = %q, want %q", got, want)
	}

	m = newTestMirror(t, 5, 3)
	writeString(t, m, "12345X")
	if got, want := m.Snapshot(), "12345\nX"; got != want {
		t.Fatalf("soft-wrapped Snapshot() = %q, want %q", got, want)
	}
}

func TestAlternateScreenRestoresMainScreen(t *testing.T) {
	m := newTestMirror(t, 12, 3)
	writeString(t, m, "main")
	writeString(t, m, "\x1b[?1049h\x1b[2J\x1b[Halt")
	if got, want := m.Snapshot(), "alt"; got != want {
		t.Fatalf("alternate Snapshot() = %q, want %q", got, want)
	}
	writeString(t, m, "\x1b[?1049l")
	if got, want := m.Snapshot(), "main"; got != want {
		t.Fatalf("restored Snapshot() = %q, want %q", got, want)
	}
}

func TestTerminalQueryDoesNotBlockWrite(t *testing.T) {
	m := newTestMirror(t, 10, 3)
	done := make(chan error, 1)
	go func() {
		_, err := m.Write([]byte("\x1b[6nOK"))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Write blocked on a terminal response")
	}
	if got, want := m.Snapshot(), "OK"; got != want {
		t.Fatalf("Snapshot() = %q, want %q", got, want)
	}
}

func TestSerializeANSIRoundTrip(t *testing.T) {
	m := newTestMirror(t, 30, 6)
	writeString(t, m, strings.Join([]string{
		"\x1b[1;31mred bold\x1b[0m",
		"\r\n\x1b[38;5;202mindexed\x1b[0m",
		"\r\n\x1b[38;2;12;34;56;48;2;90;80;70mtruecolor\x1b[0m",
		"\r\nwide: 界🙂 e\u0301",
		"\x1b[5;24Htail",
	}, ""))

	serialized := m.SerializeANSI()
	if serialized == "" {
		t.Fatal("SerializeANSI returned an empty stream")
	}

	clone := newTestMirror(t, 30, 6)
	writeString(t, clone, serialized)
	if got, want := clone.Snapshot(), m.Snapshot(); got != want {
		t.Fatalf("round-trip Snapshot() = %q, want %q\nANSI: %q", got, want, serialized)
	}
}

func TestASCIICombiningGrapheme(t *testing.T) {
	m := newTestMirror(t, 20, 2)
	writeString(t, m, "e\u0301 A\u030a")
	if got, want := m.Snapshot(), "e\u0301 A\u030a"; got != want {
		t.Fatalf("Snapshot() = %q, want %q", got, want)
	}

	fragmented := newTestMirror(t, 20, 2)
	writeString(t, fragmented, "e")
	writeString(t, fragmented, "\u0301")
	writeString(t, fragmented, "!")
	if got, want := fragmented.Snapshot(), "e\u0301!"; got != want {
		t.Fatalf("fragmented Snapshot() = %q, want %q", got, want)
	}
}

func TestFragmentedUTF8(t *testing.T) {
	m := newTestMirror(t, 20, 2)
	raw := []byte("界🙂")
	for _, fragment := range [][]byte{raw[:1], raw[1:4], raw[4:6], raw[6:]} {
		if n, err := m.Write(fragment); err != nil || n != len(fragment) {
			t.Fatalf("Write(%x) = (%d, %v), want (%d, nil)", fragment, n, err, len(fragment))
		}
	}
	if got, want := m.Snapshot(), "界🙂"; got != want {
		t.Fatalf("Snapshot() = %q, want %q", got, want)
	}
}

func TestFullResetClearsTrackedPenState(t *testing.T) {
	m := newTestMirror(t, 20, 2)
	writeString(t, m, "\x1b[31mred\x1bcplain")
	if got := m.ReflowTo(20); strings.Contains(got, "\x1b[31m") {
		t.Fatalf("ReflowTo retained SGR across RIS: %q", got)
	}
}

func TestScrollRegion(t *testing.T) {
	m := newTestMirror(t, 5, 4)
	writeString(t, m, "one\r\ntwo\r\ntri\r\nfou")
	writeString(t, m, "\x1b[2;4r\x1b[4;1H\nnew")
	if got, want := m.Snapshot(), "one\ntri\nfou\nnew"; got != want {
		t.Fatalf("Snapshot() = %q, want %q", got, want)
	}
}

func TestOutOfBoundsScrollRegionCannotPanicReplay(t *testing.T) {
	m := newTestMirror(t, 80, 10)

	// A runner can retain output generated against an older, taller PTY while
	// reporting its current smaller viewport during daemon discovery. x/vt
	// accepts the stale bottom margin and panics on the reverse-index.
	writeString(t, m, "\x1b[1;50r\x1bMOK")

	if got, want := m.Snapshot(), "OK"; got != want {
		t.Fatalf("Snapshot() = %q, want %q", got, want)
	}
}

func TestSerializeANSIAfterViewportScroll(t *testing.T) {
	m := newTestMirror(t, 5, 3)
	writeString(t, m, "aaaaaB\r\nC\r\nD")
	if m.isSoftWrappedLine(0) {
		t.Fatal("soft-wrap marker did not scroll out with its row")
	}

	clone := newTestMirror(t, 5, 3)
	writeString(t, clone, m.SerializeANSI())
	if got, want := clone.Snapshot(), m.Snapshot(); got != want {
		t.Fatalf("round-trip Snapshot() = %q, want %q\nANSI: %q", got, want, m.SerializeANSI())
	}
}

func TestSerializeANSIWithScrollbackRestoresHistoryAndViewport(t *testing.T) {
	m := newTestMirror(t, 8, 3)
	writeString(t, m, "old one\r\nold two\r\ncurrent1\r\ncurrent2")
	if got := m.term.ScrollbackLen(); got != 1 {
		t.Fatalf("source scrollback length = %d, want 1", got)
	}

	clone := newTestMirror(t, 8, 3)
	writeString(t, clone, m.SerializeANSIWithScrollback())
	if got, want := clone.term.ScrollbackLen(), m.term.ScrollbackLen(); got != want {
		t.Fatalf("round-trip scrollback length = %d, want %d", got, want)
	}
	if got, want := clone.term.Scrollback().Line(0).String(), m.term.Scrollback().Line(0).String(); got != want {
		t.Fatalf("round-trip scrollback line = %q, want %q", got, want)
	}
	if got, want := clone.Snapshot(), m.Snapshot(); got != want {
		t.Fatalf("round-trip Snapshot() = %q, want %q", got, want)
	}
}

func TestSerializeANSIWithScrollbackLeavesViewportSnapshotUnchanged(t *testing.T) {
	m := newTestMirror(t, 8, 3)
	writeString(t, m, "old one\r\nold two\r\ncurrent1\r\ncurrent2")

	if got := m.SerializeANSI(); strings.Contains(got, "old one") {
		t.Fatalf("viewport-only SerializeANSI leaked scrollback: %q", got)
	}
}
