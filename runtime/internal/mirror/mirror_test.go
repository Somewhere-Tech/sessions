package mirror

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

// The daemon writes every PTY chunk through this path at 300x50, so the cost
// of one printable token is multiplied by everything a session prints.
func BenchmarkWritePrintableRun(b *testing.B) {
	chunk := []byte(strings.Repeat("sessions mirror throughput sample line 0123456789\r\n", 8))
	m, err := NewSize(DefaultCols, DefaultRows)
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()
	b.SetBytes(int64(len(chunk)))
	b.ResetTimer()
	for b.Loop() {
		if _, err := m.Write(chunk); err != nil {
			b.Fatal(err)
		}
	}
}

func TestProtectASCIICombiningReportsWhetherItChangedTheToken(t *testing.T) {
	for _, plain := range []string{"a", "abc", "界", "e", " "} {
		got, changed := protectASCIICombining([]byte(plain))
		if changed || string(got) != plain {
			t.Errorf("protectASCIICombining(%q) = (%q, %t), want the input unchanged", plain, got, changed)
		}
	}
	got, changed := protectASCIICombining([]byte("é"))
	if !changed {
		t.Fatalf("protectASCIICombining(%q) reported no change", "é")
	}
	if want := string(protectedASCIIBase+'e') + "́"; string(got) != want {
		t.Fatalf("protectASCIICombining = %q, want %q", got, want)
	}
}

// Skipping restoreProtectedASCII for unprotected tokens is only safe if the
// screen never carries a protected code point between writes. Assert exactly
// that: no cell holds one, and running the pass unconditionally afterwards
// cannot change what the mirror reports.
func assertNoProtectedResidue(t *testing.T, m *Mirror) {
	t.Helper()
	snapshot, serialized, reflow := m.Snapshot(), m.SerializeANSI(), m.ReflowTo(60)
	m.mu.Lock()
	for y := 0; y < m.rows; y++ {
		for x := 0; x < m.cols; x++ {
			cell := m.term.CellAt(x, y)
			if cell == nil || cell.Content == "" {
				continue
			}
			r, _ := utf8.DecodeRuneInString(cell.Content)
			if r >= protectedASCIIBase+0x20 && r < protectedASCIIBase+0x7f {
				m.mu.Unlock()
				t.Fatalf("cell (%d,%d) = %q kept a protected ASCII base", x, y, cell.Content)
			}
		}
	}
	m.restoreProtectedASCII()
	m.mu.Unlock()
	if got := m.Snapshot(); got != snapshot {
		t.Fatalf("an extra restore pass changed Snapshot():\n got %q\nwant %q", got, snapshot)
	}
	if got := m.SerializeANSI(); got != serialized {
		t.Fatalf("an extra restore pass changed SerializeANSI():\n got %q\nwant %q", got, serialized)
	}
	if got := m.ReflowTo(60); got != reflow {
		t.Fatalf("an extra restore pass changed ReflowTo():\n got %q\nwant %q", got, reflow)
	}
}

func TestSkippingTheRestorePassLeavesNoProtectedResidue(t *testing.T) {
	cases := map[string]string{
		"plain":               "plain ASCII with no combining marks at all\r\nsecond line\r\n",
		"ascii combining":     "é Å ñ ö\r\n",
		"mixed":               "before é after\r\nmore plain text\r\nç tail\r\n",
		"combining then bulk": "é" + strings.Repeat("x", 400),
		"wide and emoji":      "界\U0001f642 é 界\r\n",
		"scrolled":            strings.Repeat("é line\r\n", 40),
		"erased":              "é kept\x1b[H\x1b[2Jfresh",
		"alternate screen":    "main é\x1b[?1049h\x1b[2J\x1b[Halt é\x1b[?1049l",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			m := newTestMirror(t, 20, 6)
			writeString(t, m, raw)
			assertNoProtectedResidue(t, m)
		})
	}

	entries, err := os.ReadDir(filepath.Join("testdata", "recordings"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Run(entry.Name(), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "recordings", entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			m := newTestMirror(t, DefaultCols, DefaultRows)
			if _, err := m.Write(raw); err != nil {
				t.Fatal(err)
			}
			assertNoProtectedResidue(t, m)
		})
	}
}

// A protected token restores correctly even when the writes around it are
// unprotected and therefore no longer trigger a restore pass of their own.
func TestCombiningMarksSurviveSurroundingUnprotectedWrites(t *testing.T) {
	m := newTestMirror(t, 30, 3)
	writeString(t, m, "start ")
	writeString(t, m, "é")
	writeString(t, m, " middle ")
	writeString(t, m, "Å")
	writeString(t, m, " end")
	if got, want := m.Snapshot(), "start é middle Å end"; got != want {
		t.Fatalf("Snapshot() = %q, want %q", got, want)
	}
	assertNoProtectedResidue(t, m)
}

// The drain goroutine can return on its own, after which nothing reads the
// stop marker. Close must still finish instead of blocking forever while
// holding m.mu, which would wedge every other operation on that session.
func TestCloseDoesNotBlockWhenTheDrainAlreadyReturned(t *testing.T) {
	// The pipe stays open with no reader left: this is the state in which a
	// stop-marker write under m.mu blocks forever and wedges every operation on
	// the session's mirror. The other way a drain can exit — its emulator read
	// failing — cannot be simulated here, because reaching it from a test means
	// closing the emulator while its own goroutine is inside vt's
	// unsynchronized Read; Close itself never does that, it waits for drainDone
	// first.
	cases := map[string]func(t *testing.T, m *Mirror){
		"drain returned with the pipe still open": func(t *testing.T, m *Mirror) {
			if _, err := io.WriteString(m.term.InputPipe(), drainStopMarker); err != nil {
				t.Fatal(err)
			}
			<-m.drainDone
		},
	}
	for name, wedge := range cases {
		t.Run(name, func(t *testing.T) {
			m, err := NewSize(20, 3)
			if err != nil {
				t.Fatal(err)
			}
			wedge(t, m)

			done := make(chan error, 1)
			go func() { done <- m.Close() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Close after an early drain exit: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Close blocked after the drain goroutine had already returned")
			}

			// A wedged Close would also have held m.mu forever; every other
			// operation must remain answerable.
			if _, err := m.Write([]byte("x")); err == nil {
				t.Fatal("Write after Close should report a closed mirror")
			}
			if got := m.Snapshot(); got != "" {
				t.Fatalf("Snapshot after Close = %q", got)
			}
			if err := m.Close(); err != nil {
				t.Fatalf("second Close: %v", err)
			}
		})
	}
}

func TestCloseUnblocksOperationsAndIsIdempotent(t *testing.T) {
	m, err := NewSize(20, 3)
	if err != nil {
		t.Fatal(err)
	}
	writeString(t, m, "hello")
	// A terminal query queues a reply the drain must consume before Close.
	writeString(t, m, "\x1b[c\x1b[6n")
	done := make(chan error, 2)
	go func() { done <- m.Close() }()
	go func() { done <- m.Close() }()
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Close: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Close blocked with a pending terminal reply")
		}
	}
	if err := m.Resize(10, 2); err == nil {
		t.Fatal("Resize after Close should report a closed mirror")
	}
}
