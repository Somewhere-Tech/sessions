package mirror

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

var (
	sgrRE           = regexp.MustCompile(`^\x1b\[[0-9;]*m`)
	csiRE           = regexp.MustCompile(`^\x1b\[[0-?]*[ -/]*[@-~]`)
	oscRE           = regexp.MustCompile(`^\x1b\][^\x07]*\x07`)
	cursorForwardRE = regexp.MustCompile(`\x1b\[(\d+)C`)
)

type reflowGlyph struct {
	sgr string
	ch  string
	w   int
}

// ReflowANSI is the Go port of runtime/testdata/node-runtime/src/reflow.ts. It wraps prose at width,
// preserves hard newlines and structural box/table rows, expands serialized
// cursor-forward gaps, and carries SGR state over inserted line breaks.
func ReflowANSI(text string, width int) string {
	normalized := expandCursorForward(text)
	normalized = strings.ReplaceAll(normalized, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = reflowLine(line, width, true, 8)
	}
	return strings.Join(out, "\r\n")
}

func stripANSI(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\x1b' {
			if match := matchEscape(s[i:], csiRE, oscRE); match != "" {
				i += len(match)
				continue
			}
			i++
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		if size == 0 {
			break
		}
		out.WriteString(s[i : i+size])
		i += size
	}
	return out.String()
}

func expandCursorForward(s string) string {
	return cursorForwardRE.ReplaceAllStringFunc(s, func(match string) string {
		parts := cursorForwardRE.FindStringSubmatch(match)
		n, err := strconv.Atoi(parts[1])
		if err != nil || n <= 0 {
			return ""
		}
		return strings.Repeat(" ", n)
	})
}

func charWidth(r rune) int {
	cp := int(r)
	if cp == 0 || cp < 0x20 || (cp >= 0x7f && cp < 0xa0) {
		return 0
	}
	if cp >= 0x0300 && cp <= 0x036f {
		return 0
	}
	if (cp >= 0x1100 && cp <= 0x115f) ||
		(cp >= 0x2e80 && cp <= 0x303e) ||
		(cp >= 0x3041 && cp <= 0x33ff) ||
		(cp >= 0x3400 && cp <= 0x4dbf) ||
		(cp >= 0x4e00 && cp <= 0x9fff) ||
		(cp >= 0xa000 && cp <= 0xa4cf) ||
		(cp >= 0xac00 && cp <= 0xd7a3) ||
		(cp >= 0xf900 && cp <= 0xfaff) ||
		(cp >= 0xfe30 && cp <= 0xfe4f) ||
		(cp >= 0xff00 && cp <= 0xff60) ||
		(cp >= 0xffe0 && cp <= 0xffe6) ||
		(cp >= 0x1f300 && cp <= 0x1faff) ||
		(cp >= 0x20000 && cp <= 0x3fffd) {
		return 2
	}
	return 1
}

func visibleWidth(s string) int {
	w := 0
	for _, r := range s {
		w += charWidth(r)
	}
	return w
}

func hasBoxDrawing(s string) bool {
	for _, r := range s {
		if (r >= '─' && r <= '╿') || (r >= '▀' && r <= '▟') {
			return true
		}
	}
	return false
}

func isPipeTableRow(s string) bool {
	trimmed := strings.TrimRight(s, " \t\r\n\v\f")
	return strings.HasSuffix(trimmed, "|") && strings.Count(trimmed, "|") >= 3
}

func tokenizeLine(line string) []reflowGlyph {
	glyphs := make([]reflowGlyph, 0, utf8.RuneCountInString(line))
	sgr := ""
	for i := 0; i < len(line); {
		if line[i] == '\x1b' {
			tail := line[i:]
			if match := sgrRE.FindString(tail); match != "" {
				inner := match[2 : len(match)-1]
				if inner == "" || inner == "0" {
					sgr = ""
				} else {
					sgr += match
				}
				i += len(match)
				continue
			}
			if match := matchEscape(tail, csiRE, oscRE); match != "" {
				i += len(match)
				continue
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(line[i:])
		if size == 0 {
			break
		}
		ch := line[i : i+size]
		glyphs = append(glyphs, reflowGlyph{sgr: sgr, ch: ch, w: charWidth(r)})
		i += size
	}
	return glyphs
}

func matchEscape(s string, patterns ...*regexp.Regexp) string {
	for _, pattern := range patterns {
		if match := pattern.FindString(s); match != "" {
			return match
		}
	}
	return ""
}

func renderGlyphs(glyphs []reflowGlyph) string {
	var out strings.Builder
	active := ""
	for _, glyph := range glyphs {
		if glyph.sgr != active {
			if active != "" {
				out.WriteString("\x1b[0m")
			}
			out.WriteString(glyph.sgr)
			active = glyph.sgr
		}
		out.WriteString(glyph.ch)
	}
	if active != "" {
		out.WriteString("\x1b[0m")
	}
	return out.String()
}

func wrapGlyphs(glyphs []reflowGlyph, width, indentWidth int) [][]reflowGlyph {
	if len(glyphs) == 0 {
		return [][]reflowGlyph{{}}
	}
	lines := make([][]reflowGlyph, 0, 1)
	cur := make([]reflowGlyph, 0, len(glyphs))
	curWidth := 0
	lastBreak := -1
	firstLine := true
	limit := func() int {
		if firstLine {
			return width
		}
		return max(1, width-indentWidth)
	}

	for _, glyph := range glyphs {
		if curWidth+glyph.w > limit() && len(cur) > 0 {
			if lastBreak >= 0 {
				head := append([]reflowGlyph(nil), cur[:lastBreak]...)
				tail := append([]reflowGlyph(nil), cur[lastBreak+1:]...)
				lines = append(lines, head)
				cur = tail
				curWidth = glyphWidth(tail)
				lastBreak = -1
				for j := range cur {
					if cur[j].ch == " " {
						lastBreak = j
					}
				}
			} else {
				lines = append(lines, cur)
				cur = nil
				curWidth = 0
			}
			firstLine = false
		}
		cur = append(cur, glyph)
		curWidth += glyph.w
		if glyph.ch == " " {
			lastBreak = len(cur) - 1
		}
	}
	lines = append(lines, cur)
	return lines
}

func glyphWidth(glyphs []reflowGlyph) int {
	w := 0
	for _, glyph := range glyphs {
		w += glyph.w
	}
	return w
}

func reflowLine(line string, width int, preserveIndent bool, tabWidth int) string {
	width = max(4, width)
	if line == "" {
		return ""
	}
	if strings.ContainsRune(line, '\t') {
		line = strings.ReplaceAll(line, "\t", strings.Repeat(" ", tabWidth))
	}

	plain := stripANSI(line)
	if strings.TrimSpace(plain) == "" {
		return ""
	}
	plainWidth := visibleWidth(plain)
	if plainWidth > width && (hasBoxDrawing(plain) || isPipeTableRow(plain)) {
		return line
	}

	indentText := ""
	if preserveIndent {
		for _, r := range plain {
			if r != ' ' && r != '\t' && r != '\r' && r != '\n' && r != '\v' && r != '\f' {
				break
			}
			indentText += string(r)
		}
	}
	if visibleWidth(indentText) > width/2 {
		indentText = ""
	}
	indentWidth := visibleWidth(indentText)

	wrapped := wrapGlyphs(tokenizeLine(line), width, indentWidth)
	for i := range wrapped {
		segmentLimit := width
		if i > 0 {
			segmentLimit = max(1, width-indentWidth)
		}
		segmentWidth := glyphWidth(wrapped[i])
		if segmentWidth < segmentLimit && len(wrapped[i]) > 0 {
			trailingSGR := wrapped[i][len(wrapped[i])-1].sgr
			for segmentWidth < segmentLimit {
				wrapped[i] = append(wrapped[i], reflowGlyph{sgr: trailingSGR, ch: " ", w: 1})
				segmentWidth++
			}
		}
	}

	out := make([]string, len(wrapped))
	for i, segment := range wrapped {
		out[i] = renderGlyphs(segment)
		if i > 0 {
			out[i] = indentText + out[i]
		}
	}
	return strings.Join(out, "\r\n")
}
