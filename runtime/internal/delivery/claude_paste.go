package delivery

import (
	"regexp"
	"strings"
)

var claudePasteOpening = regexp.MustCompile(`^<pasted_content(?: id="[A-Za-z0-9_-]+")?>`)

// ClaudePastedText recognizes one complete outer envelope from Claude's
// persisted user event. Claude also repeats the id on the closing tag, which
// is not XML. Never search for a matching substring inside unrelated input.
// The original event remains intact in history and durable transcript mirrors.
func ClaudePastedText(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	opening := claudePasteOpening.FindString(trimmed)
	if opening == "" {
		return "", false
	}
	closing := "</" + opening[1:]
	if !strings.HasSuffix(trimmed, closing) {
		// Some Claude versions use an ordinary closing tag instead.
		closing = "</pasted_content>"
		if !strings.HasSuffix(trimmed, closing) {
			return "", false
		}
	}
	if len(trimmed) < len(opening)+len(closing) {
		return "", false
	}
	return trimmed[len(opening) : len(trimmed)-len(closing)], true
}
