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
	"unicode"
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

const (
	// DefaultCols and DefaultRows are the canonical PTY dimensions pinned by
	// the TypeScript daemon.
	DefaultCols = 300
	DefaultRows = 50

	// The TypeScript mirrors retain 5000 rows. Snapshot deliberately exposes
	// only the active viewport, but keeping the same history makes terminal
	// behavior (notably ED and normal-buffer restoration) match xterm-headless.
	defaultScrollback = 5000
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

	term := vt.NewEmulator(cols, rows)
	term.SetScrollbackSize(defaultScrollback)
	m := &Mirror{
		term:      term,
		cols:      cols,
		rows:      rows,
		drainDone: make(chan struct{}),
		autoWrap:  true,
		scrollBot: rows - 1,
	}
	m.wrapped[0] = make([]bool, rows)
	m.wrapped[1] = make([]bool, rows)

	// Some terminal queries produce replies. vt exposes those through an
	// io.Pipe, whose writer intentionally blocks until it has a reader. The
	// mirror is observational and never sends replies back to the PTY, so drain
	// them to keep DA/DSR/OSC queries from stalling Write.
	go func() {
		defer close(m.drainDone)
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

	return m, nil
}

// Close releases the emulator and its terminal-response drain. A daemon owns
// a mirror for the life of its session; short-lived tools and tests should
// close theirs.
func (m *Mirror) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	if _, err := io.WriteString(m.term.InputPipe(), drainStopMarker); err != nil {
		return err
	}
	<-m.drainDone
	return m.term.Close()
}

// Write parses raw PTY output and updates the terminal. It implements
// io.Writer so callers can feed a PTY directly into a Mirror.
func (m *Mirror) Write(raw []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, io.ErrClosedPipe
	}
	if err := m.writeTracked(raw); err != nil {
		return 0, err
	}
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

// ReflowTo serializes the active screen and applies the exact server-side
// reflow semantics preserved in runtime/testdata/node-runtime/src/reflow.ts.
func (m *Mirror) ReflowTo(width int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	serialized := m.serializeANSI()
	if m.altScreen {
		// addon-serialize restores the active cursor pen before switching to
		// the alternate buffer. The TS reflow drops that mode sequence but
		// intentionally retains the SGR prefix, so preserve the same input.
		serialized = m.mainANSI + m.currentSGR + "\x1b[?1049h\x1b[H" + serialized
	}
	return ReflowANSI(serialized, width)
}

func (m *Mirror) serializeANSI() string {
	lastX := make([]int, m.rows)
	lastY := -1
	for y := 0; y < m.rows; y++ {
		lastX[y] = -1
		for x := m.cols - 1; x >= 0; x-- {
			cell := m.term.CellAt(x, y)
			if cell == nil || cell.Width == 0 {
				continue
			}
			// Default untouched cells are ordinary unstyled spaces in x/vt.
			// A styled space is meaningful (usually an erased background run).
			if (cell.Content != "" && cell.Content != " ") ||
				!cell.Style.IsZero() || cell.Link.URL != "" {
				lastX[y] = x
				lastY = y
				break
			}
		}
	}
	if lastY < 0 {
		return ""
	}

	var out strings.Builder
	var activeStyle uv.Style
	activeLinkURL := ""
	activeLinkParams := ""
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
			if cell.Link.URL != "" && style.Underline == uv.UnderlineNone {
				// xterm presents OSC 8 links as underlined cells, and its addon
				// serializes that visual attribute as SGR 4. Retain the OSC 8
				// target while also matching the serialized/reflowed appearance.
				style.Underline = uv.UnderlineSingle
			}
			if !style.Equal(&activeStyle) {
				out.WriteString(style.Diff(&activeStyle))
				activeStyle = style
			}

			if cell.Link.URL != activeLinkURL || cell.Link.Params != activeLinkParams {
				if activeLinkURL != "" {
					out.WriteString("\x1b]8;;\x07")
				}
				if cell.Link.URL != "" {
					out.WriteString("\x1b]8;")
					out.WriteString(cell.Link.Params)
					out.WriteByte(';')
					out.WriteString(cell.Link.URL)
					out.WriteByte('\x07')
				}
				activeLinkURL = cell.Link.URL
				activeLinkParams = cell.Link.Params
			}

			if cell.Content == "" {
				out.WriteByte(' ')
			} else {
				out.WriteString(cell.Content)
			}
		}
	}
	if activeLinkURL != "" {
		out.WriteString("\x1b]8;;\x07")
	}
	if !activeStyle.IsZero() {
		out.WriteString("\x1b[0m")
	}
	return out.String()
}

func (m *Mirror) isSoftWrappedLine(y int) bool {
	wraps := m.activeWraps()
	return y >= 0 && y < len(wraps) && wraps[y]
}
