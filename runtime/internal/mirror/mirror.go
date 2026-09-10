// Package mirror maintains a headless terminal screen and exposes the
// representations used by the sessions daemon.
package mirror

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

const (
	// DefaultCols and DefaultRows are the canonical PTY dimensions.
	DefaultCols = 300
	DefaultRows = 50

	// Mirrors retain 5000 rows. Snapshot deliberately exposes
	// only the active viewport, but keeping the same history makes terminal
	// behavior (notably ED and normal-buffer restoration) match xterm-headless.
	defaultScrollback = 5000

	// DefaultScrollbackBudgetBytes bounds retained history by size as well as by
	// line count. Five thousand lines is cheap when they are shell prompts and
	// expensive when they are full-width: a retained cell costs on the order of
	// a hundred bytes, so the same five thousand lines measured 5 MiB narrow and
	// about 200 MiB at 300 columns. The budget is what makes the worst case the
	// same shape as the ordinary one.
	DefaultScrollbackBudgetBytes = 2 * 1024 * 1024

	// scrollbackCellBytes is the retained size of one uv.Cell: a string header,
	// a style, a link holding two more string headers, and a width. Measured at
	// 104 to 136 bytes depending on content; the estimate is deliberately not
	// exact, because it only decides where a budget falls.
	scrollbackCellBytes = 128
)

var errInvalidSize = errors.New("mirror: terminal dimensions must be positive")

const drainStopMarker = "\x00sessions-mirror:close:4f75f76c\x00"

const protectedASCIIBase rune = 0xf0000

// Mirror is a concurrency-safe, pure-Go terminal mirror. Writes are parsed as
// raw PTY output; reads always describe the currently active viewport.
type Mirror struct {
	mu   sync.Mutex
	term *vt.Emulator
	cols int
	rows int

	drainDone chan struct{}
	closed    bool

	// The ANSI stream that rebuilds this screen and its history, kept while the
	// emulator is released. Empty whenever the emulator is present.
	hibernated string
	// When this mirror was last written to or read from, which is what decides
	// whether anyone still needs its emulator.
	touched time.Time

	// The retained-history budget in bytes, with a running estimate of what the
	// retained history costs and the line count that estimate covers.
	scrollbackBudget int
	budgetBytes      int
	budgetCountedLen int

	// x/vt does not expose xterm's per-line isWrapped bit. This tiny parser
	// tracks only sequence boundaries and pending auto-wrap transitions; x/vt
	// remains the source of truth for all terminal operations and cells.
	trackState vtTrackState
	csi        []byte
	phantom    bool
	autoWrap   bool
	altScreen  bool
	wrapped    [2][]bool
	currentSGR string
	mainANSI   string
	scrollTop  int
	scrollBot  int
	sequenceY  int
}

type vtTrackState uint8

const (
	trackGround vtTrackState = iota
	trackEscape
	trackCSI
	trackOSC
	trackOSCEscape
	trackString
	trackStringEscape
)

// New creates a mirror at the daemon's canonical 300x50 dimensions.
func New() *Mirror {
	m, err := NewSize(DefaultCols, DefaultRows)
	if err != nil {
		panic(err) // constants above are known-valid
	}
	return m
}

// NewSize creates a mirror with explicit dimensions. It exists for focused
// tests and serialization consumers; production callers should normally use
// New.
func NewSize(cols, rows int) (*Mirror, error) {
	if cols <= 0 || rows <= 0 {
		return nil, errInvalidSize
	}

	m := &Mirror{
		cols:      cols,
		rows:      rows,
		autoWrap:  true,
		scrollBot: rows - 1,
		touched:   time.Now(),

		scrollbackBudget: DefaultScrollbackBudgetBytes,
	}
	m.wrapped[0] = make([]bool, rows)
	m.wrapped[1] = make([]bool, rows)
	m.startEmulatorLocked()
	return m, nil
}

// startEmulatorLocked builds the emulator and the goroutine that drains its
// replies. Some terminal queries produce replies. vt exposes those through an
// io.Pipe, whose writer intentionally blocks until it has a reader. The mirror
// is observational and never sends replies back to the PTY, so drain them to
// keep DA/DSR/OSC queries from stalling Write.
func (m *Mirror) startEmulatorLocked() {
	term := vt.NewEmulator(m.cols, m.rows)
	term.SetScrollbackSize(defaultScrollback)
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		buf := make([]byte, 4096)
		pending := make([]byte, 0, len(drainStopMarker)*2)
		for {
			n, err := term.Read(buf)
			if n > 0 {
				pending = append(pending, buf[:n]...)
				if bytes.Contains(pending, []byte(drainStopMarker)) {
					return
				}
				if len(pending) > len(drainStopMarker) {
					pending = append(pending[:0], pending[len(pending)-len(drainStopMarker):]...)
				}
			}
			if err != nil {
				return
			}
		}
	}()
	m.term = term
	m.drainDone = drainDone
}

// stopEmulator releases an emulator and its drain. It is called off the mirror
// lock, by Close and by Hibernate, and it must stay that way.
//
// The stop marker goes through vt's io.Pipe, whose writer blocks until a
// reader consumes it, and the drain goroutine can already have returned on its
// own (its Read fails, or it saw a marker). Writing under m.mu would then block
// forever and wedge every other operation on this session's mirror, so signal
// off the lock and never wait on the write itself: whichever of the two
// completes first is enough, and term.Close() below releases a writer nobody
// will ever read (vt closes the pipe with EOF).
func stopEmulator(term *vt.Emulator, drainDone chan struct{}) error {
	marker := make(chan struct{})
	go func() {
		defer close(marker)
		_, _ = io.WriteString(term.InputPipe(), drainStopMarker)
	}()
	select {
	case <-drainDone:
		// Drain already gone; nothing will consume the marker. Closing the
		// emulator is what unblocks the write above.
	case <-marker:
		// The marker was delivered (or the pipe is already closed); either way
		// the drain observes it and returns.
		<-drainDone
	}
	// Always release the emulator, including on a failed stop-marker write: a
	// mirror that reports an error but keeps its emulator and pipe alive leaks
	// both for the life of the daemon.
	return term.Close()
}

// Close releases the emulator and its terminal-response drain. A daemon owns
// a mirror for the life of its session; short-lived tools and tests should
// close theirs.
func (m *Mirror) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	term, drainDone := m.term, m.drainDone
	m.term, m.drainDone = nil, nil
	m.hibernated = ""
	m.mu.Unlock()

	// A hibernated mirror has no emulator and no drain to stop.
	if term == nil {
		return nil
	}
	return stopEmulator(term, drainDone)
}

// Hibernate releases the emulator of a mirror nobody is reading.
//
// The emulator is what a live session actually costs: about 8 MiB at 300x50,
// of which 4 MiB is an ANSI parser buffer x/vt allocates per emulator and the
// rest is its two screens. A quiet session that nobody is watching does not
// need any of it until someone reads it, so the screen and its history are kept
// as the same ANSI stream the daemon already hands a resuming client, and the
// emulator is rebuilt from that stream on the next read or write.
//
// It reports whether the mirror is hibernated afterwards. A mirror showing an
// alternate screen, sitting inside a scroll region, or holding a half-parsed
// escape sequence keeps its emulator: those are states the serialized stream
// does not carry, and a screen that came back different would be worse than a
// screen that cost memory.
func (m *Mirror) Hibernate() bool {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return false
	}
	if m.term == nil {
		m.mu.Unlock()
		return true
	}
	if !m.canHibernateLocked() {
		m.mu.Unlock()
		return false
	}
	m.hibernated = m.hibernationStreamLocked()
	term, drainDone := m.term, m.drainDone
	m.term, m.drainDone = nil, nil
	m.mu.Unlock()
	_ = stopEmulator(term, drainDone)
	return true
}

// SetScrollbackBudget changes how many bytes of history this mirror retains.
// A budget of zero or less keeps only the line cap.
func (m *Mirror) SetScrollbackBudget(bytes int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scrollbackBudget = bytes
	m.budgetBytes, m.budgetCountedLen = 0, 0
	m.enforceScrollbackBudgetLocked()
}

// enforceScrollbackBudgetLocked drops the oldest history once it costs more
// than the budget allows.
//
// A line's size is its cell count, which is a slice length rather than a walk
// of its contents, so the running total costs one addition per line that
// scrolls. Dropping costs more: x/vt's own trim reslices its line list, which
// leaves the dropped lines reachable through the array behind it, so the kept
// lines are pushed into a cleared scrollback instead. That happens only when
// the budget is actually exceeded.
func (m *Mirror) enforceScrollbackBudgetLocked() {
	if m.term == nil || m.scrollbackBudget <= 0 {
		return
	}
	scrollback := m.term.Scrollback()
	if scrollback == nil {
		return
	}
	lines := scrollback.Lines()
	if len(lines) < m.budgetCountedLen {
		// History shrank under it: the line cap evicted, or something cleared
		// the buffer. Count again rather than guess.
		m.budgetBytes, m.budgetCountedLen = 0, 0
	}
	for index := m.budgetCountedLen; index < len(lines); index++ {
		m.budgetBytes += len(lines[index]) * scrollbackCellBytes
	}
	m.budgetCountedLen = len(lines)
	if m.budgetBytes <= m.scrollbackBudget {
		return
	}

	// Over budget. The running total can only be too high — the line cap
	// evicts silently once history is full — so the exact size decides what
	// goes, counted backwards from the newest line a reader wants first.
	total, first := 0, 0
	for index := len(lines) - 1; index >= 0; index-- {
		total += len(lines[index]) * scrollbackCellBytes
		if total > m.scrollbackBudget {
			first = index + 1
			break
		}
	}
	if first == 0 {
		m.budgetBytes = total
		return
	}
	kept := append([]uv.Line(nil), lines[first:]...)
	scrollback.Clear()
	for _, line := range kept {
		scrollback.Push(line)
	}
	m.budgetBytes, m.budgetCountedLen = 0, 0
	for _, line := range scrollback.Lines() {
		m.budgetBytes += len(line) * scrollbackCellBytes
		m.budgetCountedLen++
	}
}

// HibernateIfIdle releases the emulator when nothing has written to or read
// from this mirror for quiet. It reports whether the mirror is hibernated
// afterwards.
//
// Idleness, rather than a count of viewers, is what says the emulator is not
// needed: a client that is streaming output reads the mirror once when it
// attaches, and a session whose runner is producing output wakes the mirror on
// the next write anyway.
func (m *Mirror) HibernateIfIdle(quiet time.Duration) bool {
	m.mu.Lock()
	idle := time.Since(m.touched) >= quiet
	hibernated := m.term == nil && !m.closed
	m.mu.Unlock()
	if hibernated {
		return true
	}
	if !idle {
		return false
	}
	return m.Hibernate()
}

// Hibernating reports whether this mirror currently holds no emulator.
func (m *Mirror) Hibernating() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.closed && m.term == nil
}

func (m *Mirror) canHibernateLocked() bool {
	return !m.altScreen &&
		m.trackState == trackGround &&
		!m.phantom &&
		m.scrollTop == 0 && m.scrollBot == m.rows-1
}

// hibernationStreamLocked is the stream that rebuilds this mirror.
//
// It follows the same shape as SerializeANSIWithScrollback — history, filler
// rows to push it into scrollback, then the repainted viewport — but it renders
// the history lines itself. uv.Line.Render writes OSC 8 hyperlinks with the URL
// in the parameter field, which a terminal reads as a hyperlink reset, so a
// history line replayed through it comes back with its links dropped. What
// clients are sent is unchanged; what the daemon feeds back to itself must
// survive the round trip.
func (m *Mirror) hibernationStreamLocked() string {
	scrollback := m.term.Scrollback()
	var out strings.Builder
	if scrollback != nil && scrollback.Len() > 0 {
		for _, line := range scrollback.Lines() {
			out.WriteString(renderLineForReplay(line))
			out.WriteString("\r\n")
		}
		for row := 1; row < m.rows; row++ {
			out.WriteString("\r\n")
		}
		out.WriteString("\x1b[2J\x1b[H")
	}
	out.WriteString(m.serializeForReplay())
	out.WriteString(m.restoreCursorLocked())
	return out.String()
}

// renderLineForReplay writes one retained history line so that feeding it back
// to an emulator reproduces the same cells.
//
// Hyperlinks need care. x/vt fills Link.URL from the first OSC 8 field (the
// parameters) and Link.Params from the second (the URI), so the two are
// swapped against their names; writing them back in field order is what makes
// the round trip an identity. uv.Line.Render writes them in the other order,
// which a terminal reads as a hyperlink reset — clients keep receiving exactly
// what they receive today, but the daemon cannot feed that back to itself.
func renderLineForReplay(line uv.Line) string {
	var out strings.Builder
	var style uv.Style
	var link uv.Link
	for index := 0; index < len(line); index++ {
		cell := line.At(index)
		if cell == nil || cell.Width == 0 {
			continue
		}
		if !cell.Style.Equal(&style) {
			out.WriteString(cell.Style.Diff(&style))
			style = cell.Style
		}
		if cell.Link != link {
			if link != (uv.Link{}) {
				out.WriteString("\x1b]8;;\x07")
			}
			if cell.Link != (uv.Link{}) {
				out.WriteString("\x1b]8;")
				out.WriteString(cell.Link.URL)
				out.WriteByte(';')
				out.WriteString(cell.Link.Params)
				out.WriteByte('\x07')
			}
			link = cell.Link
		}
		if cell.Content == "" {
			out.WriteByte(' ')
		} else {
			out.WriteString(cell.Content)
		}
	}
	if link != (uv.Link{}) {
		out.WriteString("\x1b]8;;\x07")
	}
	if !style.IsZero() {
		out.WriteString("\x1b[0m")
	}
	return strings.TrimRight(out.String(), " ")
}

// restoreCursorLocked ends the hibernation stream by putting the pen and the
// cursor back. The serializer paints cells and stops; where the next character
// lands, and what it looks like, is state of its own.
func (m *Mirror) restoreCursorLocked() string {
	position := m.term.CursorPosition()
	cursor := "\x1b[" + strconv.Itoa(position.Y+1) + ";" + strconv.Itoa(position.X+1) + "H"
	if !m.autoWrap {
		cursor += "\x1b[?7l"
	}
	return m.currentSGR + cursor
}

// wakeLocked rebuilds the emulator from the stream hibernation kept. The
// wrap tracking, pen and screen mode are the mirror's own and were never
// released, so only the emulator's cells are restored here.
func (m *Mirror) wakeLocked() error {
	if m.term != nil {
		return nil
	}
	m.startEmulatorLocked()
	restored := m.hibernated
	m.hibernated = ""
	if restored == "" {
		return nil
	}
	// The replay goes through the mirror's own write path, because that path is
	// what keeps combining marks attached through x/vt. It would also rewrite
	// the wrap tracking, which the serialized stream cannot describe — every
	// row in it ends in a newline — so the tracking is put back afterwards.
	saved := m.tracking()
	m.trackState, m.csi = trackGround, nil
	err := m.writeTracked([]byte(restored))
	m.restoreTracking(saved)
	return err
}

// tracking is everything the mirror knows that the emulator does not.
type trackingState struct {
	trackState vtTrackState
	csi        []byte
	phantom    bool
	autoWrap   bool
	altScreen  bool
	wrapped    [2][]bool
	currentSGR string
	mainANSI   string
	scrollTop  int
	scrollBot  int
	sequenceY  int
}

func (m *Mirror) tracking() trackingState {
	saved := trackingState{
		trackState: m.trackState, csi: append([]byte(nil), m.csi...),
		phantom: m.phantom, autoWrap: m.autoWrap, altScreen: m.altScreen,
		currentSGR: m.currentSGR, mainANSI: m.mainANSI,
		scrollTop: m.scrollTop, scrollBot: m.scrollBot, sequenceY: m.sequenceY,
	}
	for index := range m.wrapped {
		saved.wrapped[index] = append([]bool(nil), m.wrapped[index]...)
	}
	return saved
}

func (m *Mirror) restoreTracking(saved trackingState) {
	m.trackState, m.csi = saved.trackState, saved.csi
	m.phantom, m.autoWrap, m.altScreen = saved.phantom, saved.autoWrap, saved.altScreen
	m.currentSGR, m.mainANSI = saved.currentSGR, saved.mainANSI
	m.scrollTop, m.scrollBot, m.sequenceY = saved.scrollTop, saved.scrollBot, saved.sequenceY
	m.wrapped = saved.wrapped
}

// Write parses raw PTY output and updates the terminal. It implements
// io.Writer so callers can feed a PTY directly into a Mirror.
func (m *Mirror) Write(raw []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, io.ErrClosedPipe
	}
	m.touched = time.Now()
	if err := m.wakeLocked(); err != nil {
		return 0, err
	}
	if err := m.writeTracked(raw); err != nil {
		return 0, err
	}
	m.enforceScrollbackBudgetLocked()
	return len(raw), nil
}

// Resize keeps the daemon-side mirror in lockstep with the runner PTY. The
// emulator performs the terminal reflow; the wrapper refreshes the dimensions
// used by serialization and soft-wrap tracking.
func (m *Mirror) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return errInvalidSize
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return io.ErrClosedPipe
	}
	m.touched = time.Now()
	if err := m.wakeLocked(); err != nil {
		return err
	}
	m.term.Resize(cols, rows)
	for index := range m.wrapped {
		resized := make([]bool, rows)
		copy(resized, m.wrapped[index])
		m.wrapped[index] = resized
	}
	m.cols = cols
	m.rows = rows
	m.scrollTop = 0
	m.scrollBot = rows - 1
	return nil
}

func (m *Mirror) writeTracked(raw []byte) error {
	for i := 0; i < len(raw); {
		if m.trackState != trackGround {
			if m.trackState == trackCSI && raw[i] >= 0x40 && raw[i] <= 0x7e {
				params := string(m.csi)
				if m.cancelOutOfBoundsScrollRegion(raw[i], params) {
					// x/vt currently accepts DECSTBM margins larger than the
					// viewport. A following reverse-index then passes that
					// oversized region to ultraviolet and panics. PTY replay
					// is untrusted input from the daemon's perspective, so
					// cancel the invalid CSI before its final byte reaches
					// the emulator. The tracker rejects the same region.
					if _, err := m.term.Write([]byte{0x18}); err != nil {
						return err
					}
					m.trackSequenceByte(raw[i])
					i++
					continue
				}
				if m.entersAlternateScreen(raw[i], params) {
					m.mainANSI = m.serializeANSI()
				}
			}
			if _, err := m.term.Write(raw[i : i+1]); err != nil {
				return err
			}
			m.trackSequenceByte(raw[i])
			i++
			continue
		}

		b := raw[i]
		if b == '\x1b' || b < 0x20 || b == 0x7f {
			beforeY := m.term.CursorPosition().Y
			if _, err := m.term.Write(raw[i : i+1]); err != nil {
				return err
			}
			m.trackGroundControl(b, beforeY)
			i++
			continue
		}

		var token []byte
		width := 1
		if b < utf8.RuneSelf {
			end := i + 1
			for end < len(raw) {
				r, size := utf8.DecodeRune(raw[end:])
				if r == utf8.RuneError && size == 1 || !unicode.Is(unicode.M, r) {
					break
				}
				end += size
			}
			token = raw[i:end]
		} else {
			if !utf8.FullRune(raw[i:]) {
				// Let x/vt carry its UTF-8 parser state across Write calls. Wrap
				// tracking resumes when the next complete printable arrives.
				_, err := m.term.Write(raw[i:])
				return err
			}
			cluster, clusterWidth := xansi.FirstGraphemeCluster(raw[i:], xansi.GraphemeWidth)
			if len(cluster) == 0 {
				cluster = raw[i : i+1]
			}
			token = cluster
			width = clusterWidth
		}

		start := m.term.CursorPosition()
		wrapped := m.beforePrintable(width)
		if wrapped {
			start.X = 0
		}
		if width == 0 {
			attached, err := m.attachCombining(token)
			if err != nil {
				return err
			}
			if attached {
				i += len(token)
				continue
			}
		}
		protected, wasProtected := protectASCIICombining(token)
		if _, err := m.term.Write(protected); err != nil {
			return err
		}
		if wasProtected {
			// restoreProtectedASCII walks every cell on the screen. Only a token
			// that actually carried a protected base can leave one behind, and
			// each such token is restored before the next write, so the screen is
			// already clean for the common unprotected token.
			m.restoreProtectedASCII()
		}
		m.afterPrintable(width, start.X)
		i += len(token)
	}
	return nil
}

func (m *Mirror) attachCombining(token []byte) (bool, error) {
	pos := m.term.CursorPosition()
	previousX := pos.X - 1
	if m.phantom {
		previousX = pos.X
	}
	for previousX >= 0 {
		cell := m.term.CellAt(previousX, pos.Y)
		if cell != nil && cell.Width > 0 {
			break
		}
		previousX--
	}
	if previousX < 0 {
		return false, nil
	}
	previous := m.term.CellAt(previousX, pos.Y)
	if previous == nil || previous.Content == "" {
		return false, nil
	}
	previous = previous.Clone()

	var underCursor *uv.Cell
	if current := m.term.CellAt(pos.X, pos.Y); current != nil {
		underCursor = current.Clone()
	}
	if _, err := m.term.Write(token); err != nil {
		return false, err
	}
	previous.Content += string(token)
	m.term.SetCell(previousX, pos.Y, previous)
	if previousX != pos.X {
		m.term.SetCell(pos.X, pos.Y, underCursor)
	}
	return true, nil
}

func (m *Mirror) beforePrintable(width int) bool {
	if width <= 0 || !m.phantom || !m.autoWrap {
		return false
	}
	pos := m.term.CursorPosition()
	wraps := m.activeWraps()
	if pos.Y == m.scrollBot {
		m.scrollWrapsUp(m.scrollTop, m.scrollBot, 1)
		if pos.Y > m.scrollTop {
			wraps[pos.Y-1] = true
		}
	} else if pos.Y >= 0 && pos.Y < len(wraps) {
		wraps[pos.Y] = true
	}
	return true
}

func (m *Mirror) afterPrintable(width, startX int) {
	if width <= 0 || !m.autoWrap {
		return
	}
	m.phantom = startX+width >= m.cols
}

func (m *Mirror) activeWraps() []bool {
	if m.altScreen {
		return m.wrapped[1]
	}
	return m.wrapped[0]
}

func (m *Mirror) trackGroundControl(b byte, beforeY int) {
	switch b {
	case '\x1b':
		m.trackState = trackEscape
		m.sequenceY = beforeY
	case '\n', '\v', '\f':
		if beforeY == m.scrollBot {
			m.scrollWrapsUp(m.scrollTop, m.scrollBot, 1)
		}
		m.phantom = false
	case '\b', '\r':
		m.phantom = false
	}
}

func (m *Mirror) trackSequenceByte(b byte) {
	switch m.trackState {
	case trackEscape:
		switch b {
		case '[':
			m.trackState = trackCSI
			m.csi = m.csi[:0]
		case ']':
			m.trackState = trackOSC
		case 'P', 'X', '^', '_':
			m.trackState = trackString
		default:
			if b >= 0x30 && b <= 0x7e {
				m.trackState = trackGround
				switch b {
				case 'D', 'E':
					if m.sequenceY == m.scrollBot {
						m.scrollWrapsUp(m.scrollTop, m.scrollBot, 1)
					}
				case 'M':
					if m.sequenceY == m.scrollTop {
						m.scrollWrapsDown(m.scrollTop, m.scrollBot, 1)
					}
				}
				if strings.ContainsRune("78DEM c", rune(b)) {
					m.phantom = false
				}
				if b == 'c' {
					m.clearWraps(0)
					m.clearWraps(1)
					m.altScreen = false
					m.autoWrap = true
					m.currentSGR = ""
					m.mainANSI = ""
					m.scrollTop = 0
					m.scrollBot = m.rows - 1
				}
			}
		}
	case trackCSI:
		if b >= 0x40 && b <= 0x7e {
			m.handleTrackedCSI(b, string(m.csi))
			m.trackState = trackGround
			m.csi = m.csi[:0]
		} else if b >= 0x20 {
			m.csi = append(m.csi, b)
		}
	case trackOSC:
		switch b {
		case '\x07':
			m.trackState = trackGround
		case '\x1b':
			m.trackState = trackOSCEscape
		}
	case trackOSCEscape:
		if b == '\\' {
			m.trackState = trackGround
		} else {
			m.trackState = trackOSC
		}
	case trackString:
		if b == '\x1b' {
			m.trackState = trackStringEscape
		}
	case trackStringEscape:
		if b == '\\' {
			m.trackState = trackGround
		} else {
			m.trackState = trackString
		}
	}
}

func (m *Mirror) handleTrackedCSI(final byte, params string) {
	if final == 'h' || final == 'l' {
		set := final == 'h'
		if strings.HasPrefix(params, "?") {
			for _, param := range strings.Split(strings.TrimPrefix(params, "?"), ";") {
				switch param {
				case "7":
					m.autoWrap = set
					if !set {
						m.phantom = false
					}
				case "47", "1047", "1049":
					m.altScreen = set
					m.phantom = false
					if set {
						m.clearWraps(1)
					}
				}
			}
		}
		return
	}

	switch final {
	case 'r':
		m.trackScrollRegion(params)
	case 'S':
		m.scrollWrapsUp(m.scrollTop, m.scrollBot, csiCount(params))
	case 'T':
		m.scrollWrapsDown(m.scrollTop, m.scrollBot, csiCount(params))
	case 'L':
		if m.sequenceY >= m.scrollTop && m.sequenceY <= m.scrollBot {
			m.scrollWrapsDown(m.sequenceY, m.scrollBot, csiCount(params))
		}
	case 'M':
		if m.sequenceY >= m.scrollTop && m.sequenceY <= m.scrollBot {
			m.scrollWrapsUp(m.sequenceY, m.scrollBot, csiCount(params))
		}
	}

	// SGR and terminal reports do not move the cursor or cancel pending wrap.
	if strings.ContainsRune("mncqt", rune(final)) {
		if final == 'm' {
			m.trackSGR(params)
		}
		return
	}
	m.phantom = false
	if final == 'J' && (params == "2" || params == "3") {
		index := 0
		if m.altScreen {
			index = 1
		}
		m.clearWraps(index)
	}
}

func (m *Mirror) cancelOutOfBoundsScrollRegion(final byte, params string) bool {
	if final != 'r' {
		return false
	}
	_, _, ok := parseScrollRegion(params, m.rows)
	return !ok
}

func (m *Mirror) entersAlternateScreen(final byte, params string) bool {
	if final != 'h' || m.altScreen || !strings.HasPrefix(params, "?") {
		return false
	}
	for _, param := range strings.Split(strings.TrimPrefix(params, "?"), ";") {
		if param == "47" || param == "1047" || param == "1049" {
			return true
		}
	}
	return false
}

func (m *Mirror) trackSGR(params string) {
	if params == "" {
		m.currentSGR = ""
		return
	}
	sequence := "\x1b[" + params + "m"
	reset := false
	for _, param := range strings.Split(params, ";") {
		if param == "0" {
			reset = true
			break
		}
	}
	if reset {
		m.currentSGR = sequence
	} else {
		m.currentSGR += sequence
	}
}

func (m *Mirror) clearWraps(index int) {
	clear(m.wrapped[index])
}

func (m *Mirror) trackScrollRegion(params string) {
	top, bottom, ok := parseScrollRegion(params, m.rows)
	if !ok {
		return
	}
	m.scrollTop = top
	m.scrollBot = bottom
}

func parseScrollRegion(params string, rows int) (int, int, bool) {
	parts := strings.Split(params, ";")
	top, bottom := 1, rows
	if len(parts) > 0 && parts[0] != "" {
		if parsed, err := strconv.Atoi(parts[0]); err == nil {
			top = parsed
		}
	}
	if len(parts) > 1 && parts[1] != "" {
		if parsed, err := strconv.Atoi(parts[1]); err == nil {
			bottom = parsed
		}
	}
	// Match x/vt's DECSTBM defaults for zero/negative values, but reject
	// explicit margins outside the actual viewport. x/vt currently fails to
	// perform this upper-bound check itself.
	if top < 1 {
		top = 1
	}
	if bottom < 1 {
		bottom = rows
	}
	if top > rows || bottom > rows || top >= bottom {
		return 0, 0, false
	}
	return top - 1, bottom - 1, true
}

func csiCount(params string) int {
	first := params
	if separator := strings.IndexByte(first, ';'); separator >= 0 {
		first = first[:separator]
	}
	count, err := strconv.Atoi(first)
	if err != nil || count < 1 {
		return 1
	}
	return count
}

func (m *Mirror) scrollWrapsUp(top, bottom, count int) {
	wraps := m.activeWraps()
	if top < 0 || bottom >= len(wraps) || top > bottom {
		return
	}
	count = min(count, bottom-top+1)
	copy(wraps[top:bottom-count+1], wraps[top+count:bottom+1])
	clear(wraps[bottom-count+1 : bottom+1])
}

func (m *Mirror) scrollWrapsDown(top, bottom, count int) {
	wraps := m.activeWraps()
	if top < 0 || bottom >= len(wraps) || top > bottom {
		return
	}
	count = min(count, bottom-top+1)
	copy(wraps[top+count:bottom+1], wraps[top:bottom-count+1])
	clear(wraps[top : top+count])
}

// x/vt currently flushes ASCII immediately, so a following combining mark is
// emitted as a standalone width-zero cell and then overwritten. Temporarily
// moving only such ASCII bases into the supplementary private-use area makes
// x/vt's grapheme segmenter see the complete cluster. The cell content is
// restored immediately after parsing. The second result reports whether any
// base was moved, so the caller can skip the full-screen restore pass for the
// overwhelmingly common token that needed no protection.
func protectASCIICombining(raw []byte) ([]byte, bool) {
	var out bytes.Buffer
	changed := false
	for i := 0; i < len(raw); {
		b := raw[i]
		if b >= 0x20 && b < 0x7f && i+1 < len(raw) {
			r, _ := utf8.DecodeRune(raw[i+1:])
			if unicode.Is(unicode.M, r) {
				out.WriteRune(protectedASCIIBase + rune(b))
				i++
				changed = true
				continue
			}
		}
		out.WriteByte(b)
		i++
	}
	if !changed {
		return raw, false
	}
	return out.Bytes(), true
}

func (m *Mirror) restoreProtectedASCII() {
	for y := 0; y < m.rows; y++ {
		for x := 0; x < m.cols; x++ {
			cell := m.term.CellAt(x, y)
			if cell == nil || cell.Content == "" {
				continue
			}
			r, size := utf8.DecodeRuneInString(cell.Content)
			if r < protectedASCIIBase+0x20 || r >= protectedASCIIBase+0x7f {
				continue
			}
			cell.Content = string(r-protectedASCIIBase) + cell.Content[size:]
		}
	}
}

// Snapshot returns the active viewport as plain text. Physical terminal rows
// are separated with LF. Unpainted right-hand cells and empty rows below the
// last visible row are omitted; internal and leading spaces remain intact.
// This is the text analogue of xterm's translateToString(true) per row.
func (m *Mirror) Snapshot() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.touched = time.Now()
	if m.wakeLocked() != nil {
		return ""
	}

	lines := make([]string, m.rows)
	lastLine := -1
	for y := 0; y < m.rows; y++ {
		var line strings.Builder
		for x := 0; x < m.cols; x++ {
			cell := m.term.CellAt(x, y)
			if cell == nil || cell.Width == 0 {
				continue
			}
			if cell.Content == "" {
				line.WriteByte(' ')
			} else {
				line.WriteString(cell.Content)
			}
		}
		lines[y] = strings.TrimRight(line.String(), " ")
		if lines[y] != "" {
			lastLine = y
		}
	}
	if lastLine < 0 {
		return ""
	}
	return strings.Join(lines[:lastLine+1], "\n")
}

// SerializeANSI returns an ANSI stream that paints the current active screen
// into a fresh terminal of the same dimensions. Its byte choices need not be
// identical to @xterm/addon-serialize; the resulting cells are equivalent.
func (m *Mirror) SerializeANSI() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.touched = time.Now()
	if m.wakeLocked() != nil {
		return ""
	}
	return m.serializeANSI()
}

// SerializeANSIWithScrollback returns one bulk ANSI stream containing the
// retained main-buffer history followed by the current viewport. It is used
// only to prime a real terminal view: the ordinary snapshot intentionally
// remains viewport-only for status classification and reflowed views.
//
// The rows of blank output after the retained history push exactly that
// history into a fresh terminal's scrollback. ED 2 then clears only the live
// viewport (not scrollback) before the canonical active screen is painted.
// Alternate-screen applications do not expose scrollback while that screen is
// active, so retain the existing viewport-only behavior there.
func (m *Mirror) SerializeANSIWithScrollback() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.touched = time.Now()
	m.touched = time.Now()
	if m.wakeLocked() != nil {
		return ""
	}
	return m.serializeWithScrollbackLocked()
}

func (m *Mirror) serializeWithScrollbackLocked() string {
	scrollback := m.term.Scrollback()
	if m.altScreen || scrollback == nil || scrollback.Len() == 0 {
		return m.serializeANSI()
	}

	var out strings.Builder
	for _, line := range scrollback.Lines() {
		out.WriteString(line.Render())
		out.WriteString("\r\n")
	}
	for row := 1; row < m.rows; row++ {
		out.WriteString("\r\n")
	}
	// Clear the visible filler rows without erasing the scrollback we just
	// restored, then repaint the active viewport using the proven serializer.
	out.WriteString("\x1b[2J\x1b[H")
	out.WriteString(m.serializeANSI())
	return out.String()
}

// ReflowTo serializes the active screen and applies the server-side reflow
// semantics used by the terminal snapshot path.
func (m *Mirror) ReflowTo(width int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.touched = time.Now()
	if m.wakeLocked() != nil {
		return ""
	}
	serialized := m.serializeANSI()
	if m.altScreen {
		// addon-serialize restores the active cursor pen before switching to
		// the alternate buffer. The TS reflow drops that mode sequence but
		// intentionally retains the SGR prefix, so preserve the same input.
		serialized = m.mainANSI + m.currentSGR + "\x1b[?1049h\x1b[H" + serialized
	}
	return ReflowANSI(serialized, width)
}

// serializeANSI paints the active screen for a client. serializeForReplay
// paints it for this daemon, which needs the cells back exactly as they are
// rather than as a terminal should display them: no underline stands in for a
// hyperlink, and OSC 8 fields are written in the order x/vt parses them.
func (m *Mirror) serializeANSI() string { return m.serialize(false) }

func (m *Mirror) serializeForReplay() string { return m.serialize(true) }

func (m *Mirror) serialize(replay bool) string {
	lastX, lastY := m.paintedColumns()
	if lastY < 0 {
		return ""
	}

	var out strings.Builder
	var activeStyle uv.Style
	var activeLink uv.Link
	for y := 0; y <= lastY; y++ {
		if y > 0 && !m.isSoftWrappedLine(y-1) {
			out.WriteString("\r\n")
		}
		if lastX[y] < 0 {
			continue
		}

		lineEnd := lastX[y]
		if m.isSoftWrappedLine(y) {
			lineEnd = m.cols - 1
		}
		for x := 0; x <= lineEnd; x++ {
			cell := m.term.CellAt(x, y)
			if cell == nil {
				out.WriteString("\x1b[1C")
				continue
			}
			if cell.Width == 0 { // continuation cell of a wide grapheme
				continue
			}
			style := cell.Style
			if !replay && cell.Link.URL != "" && style.Underline == uv.UnderlineNone {
				// xterm presents OSC 8 links as underlined cells, and its addon
				// serializes that visual attribute as SGR 4. Retain the OSC 8
				// target while also matching the serialized/reflowed appearance.
				style.Underline = uv.UnderlineSingle
			}
			if !style.Equal(&activeStyle) {
				out.WriteString(style.Diff(&activeStyle))
				activeStyle = style
			}
			writeLinkChange(&out, cell.Link, &activeLink, replay)
			if cell.Content == "" {
				out.WriteByte(' ')
			} else {
				out.WriteString(cell.Content)
			}
		}
	}
	if activeLink.URL != "" || (replay && activeLink.Params != "") {
		out.WriteString("\x1b]8;;\x07")
	}
	if !activeStyle.IsZero() {
		out.WriteString("\x1b[0m")
	}
	return out.String()
}

// paintedColumns reports, for every row, the last column worth serializing, and
// the last row that has one. A default untouched cell is an ordinary unstyled
// space in x/vt; a styled space is meaningful, usually an erased background run.
func (m *Mirror) paintedColumns() ([]int, int) {
	lastX := make([]int, m.rows)
	lastY := -1
	for y := 0; y < m.rows; y++ {
		lastX[y] = -1
		for x := m.cols - 1; x >= 0; x-- {
			cell := m.term.CellAt(x, y)
			if cell == nil || cell.Width == 0 {
				continue
			}
			if (cell.Content != "" && cell.Content != " ") ||
				!cell.Style.IsZero() || cell.Link.URL != "" {
				lastX[y] = x
				lastY = y
				break
			}
		}
	}
	return lastX, lastY
}

// writeLinkChange emits the OSC 8 transition between two cells, if there is one.
//
// A client is sent the parameters first and the URL second, which is the order
// the sequence is defined in. x/vt fills Link.URL from the first field and
// Link.Params from the second, so a stream this daemon means to feed back to
// itself writes them in that order instead — the round trip has to land on the
// same cells, not on the same-looking text.
func writeLinkChange(out *strings.Builder, link uv.Link, active *uv.Link, replay bool) {
	if link.URL == active.URL && link.Params == active.Params {
		return
	}
	if active.URL != "" || (replay && active.Params != "") {
		out.WriteString("\x1b]8;;\x07")
	}
	if link.URL != "" || (replay && link.Params != "") {
		out.WriteString("\x1b]8;")
		if replay {
			out.WriteString(link.URL)
			out.WriteByte(';')
			out.WriteString(link.Params)
		} else {
			out.WriteString(link.Params)
			out.WriteByte(';')
			out.WriteString(link.URL)
		}
		out.WriteByte('\x07')
	}
	*active = link
}

func (m *Mirror) isSoftWrappedLine(y int) bool {
	wraps := m.activeWraps()
	return y >= 0 && y < len(wraps) && wraps[y]
}
