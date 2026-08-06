// Package ansi removes terminal control sequences from text that is about to be
// read by a person or matched by a pattern.
//
// There were four implementations of this in the runtime and they stripped
// different things, so the same escape survived in one place and not another:
//
//   - the CLI's snapshot cleaner terminated an OSC string only at BEL, so a
//     title set with the ST terminator (ESC \) swallowed the rest of the line
//     into the match or was left in the output verbatim;
//   - the transcript cleaner and the CLI's both missed the charset designators
//     (ESC ( B) Claude emits, which leave "(B" scattered through the text once
//     the bare ESC is dropped;
//   - the Claude activity classifier's CSI pattern accepted only digits and
//     letters, so a parameterised sequence like ESC [ 1 SPACE q survived into
//     the text its spinner and footer patterns are matched against;
//   - only one of the four removed DCS/APC/PM strings and stray two-byte
//     escapes at all.
//
// Strip is the union. The order below is not incidental: OSC and the other
// string-terminated sequences have to go before the generic two-byte escape
// rule, which would otherwise consume their ESC ] introducer and leave the
// string body behind as text.
package ansi

import (
	"regexp"
	"strings"
)

var (
	// osc is an operating-system-command string, terminated by BEL or ST.
	osc = regexp.MustCompile(`\x1b\][^\x07]*(?:\x07|\x1b\\)`)
	// stringControl covers DCS, PM and APC strings, which are ST-terminated
	// and may span lines.
	stringControl = regexp.MustCompile(`(?s)\x1b[P^_].*?\x1b\\`)
	// csi is the full control-sequence grammar: parameter bytes, intermediate
	// bytes, then any final byte.
	csi = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	// charset designates a character set, e.g. ESC ( B for US-ASCII.
	charset = regexp.MustCompile(`\x1b[()*+][0-9A-Za-z]`)
	// twoByte is any remaining single-character escape.
	twoByte = regexp.MustCompile(`\x1b[@-_]`)
)

// Strip removes terminal control sequences and the non-printing characters they
// leave behind, keeping tab, newline and carriage return because those carry
// the layout a reader still needs.
func Strip(text string) string {
	text = osc.ReplaceAllString(text, "")
	text = stringControl.ReplaceAllString(text, "")
	text = csi.ReplaceAllString(text, "")
	text = charset.ReplaceAllString(text, "")
	text = twoByte.ReplaceAllString(text, "")
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' || r >= 0x20 {
			return r
		}
		return -1
	}, text)
}
