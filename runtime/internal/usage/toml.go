package usage

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// This file carries a deliberately small TOML reader. The runtime has no TOML
// dependency (see runtime/go.mod) and pricing must not be the reason we grow
// the dependency surface, but a line scanner cannot read a Codex config.toml
// correctly: it cannot tell a root key from a `[profiles.cheap]` key, and it
// mistakes a `#` inside a quoted value for a comment. Both mistakes silently
// misprice every Codex event, so the parser below is table-aware instead.
//
// It keeps only what pricing asks of it - scalar values rendered as text,
// addressed by table path - and it is deliberately lenient: on malformed input
// it stops and returns what it understood, which leaves pricing on the
// conservative "no tier found" path rather than on a guess.

// tomlDocument maps a dotted table path ("" for the root table) to that
// table's scalar values.
type tomlDocument map[string]map[string]string

// value returns a scalar from one table. Missing tables and missing keys are
// indistinguishable on purpose: both mean "this file did not say".
func (d tomlDocument) value(table, key string) (string, bool) {
	values, ok := d[table]
	if !ok {
		return "", false
	}
	result, ok := values[key]
	return result, ok
}

func parseTOML(source string) tomlDocument {
	scanner := &tomlScanner{source: source, document: tomlDocument{}}
	scanner.run()
	return scanner.document
}

type tomlScanner struct {
	source   string
	pos      int
	table    string // current table path, "" for the root table
	document tomlDocument
}

func (s *tomlScanner) run() {
	for {
		s.skipIgnorable()
		if s.pos >= len(s.source) {
			return
		}
		if s.source[s.pos] == '[' {
			if !s.tableHeader() {
				return
			}
			continue
		}
		if !s.keyValue(s.table) {
			return
		}
	}
}

// skipIgnorable consumes whitespace, newlines and comments. Comments are only
// recognised here, never inside a string, which is the truncation bug the
// previous line scanner had.
func (s *tomlScanner) skipIgnorable() {
	for s.pos < len(s.source) {
		switch s.source[s.pos] {
		case ' ', '\t', '\r', '\n':
			s.pos++
		case '#':
			for s.pos < len(s.source) && s.source[s.pos] != '\n' {
				s.pos++
			}
		default:
			return
		}
	}
}

func (s *tomlScanner) skipSpaces() {
	for s.pos < len(s.source) && (s.source[s.pos] == ' ' || s.source[s.pos] == '\t') {
		s.pos++
	}
}

func (s *tomlScanner) tableHeader() bool {
	s.pos++ // '['
	arrayOfTables := s.pos < len(s.source) && s.source[s.pos] == '['
	if arrayOfTables {
		s.pos++
	}
	parts, ok := s.keyPath()
	if !ok {
		return false
	}
	s.skipSpaces()
	if s.pos >= len(s.source) || s.source[s.pos] != ']' {
		return false
	}
	s.pos++
	if arrayOfTables {
		if s.pos >= len(s.source) || s.source[s.pos] != ']' {
			return false
		}
		s.pos++
	}
	// Repeated [[array]] elements collapse onto one path. Pricing never reads
	// an array of tables, and collapsing keeps the reader small.
	s.table = strings.Join(parts, ".")
	return true
}

func (s *tomlScanner) keyValue(table string) bool {
	parts, ok := s.keyPath()
	if !ok {
		return false
	}
	s.skipSpaces()
	if s.pos >= len(s.source) || s.source[s.pos] != '=' {
		return false
	}
	s.pos++
	s.skipSpaces()
	target := table
	if len(parts) > 1 {
		target = joinTablePath(table, parts[:len(parts)-1])
	}
	return s.value(target, parts[len(parts)-1])
}

func (s *tomlScanner) keyPath() ([]string, bool) {
	parts := make([]string, 0, 2)
	for {
		s.skipSpaces()
		part, ok := s.keyPart()
		if !ok {
			return nil, false
		}
		parts = append(parts, part)
		s.skipSpaces()
		if s.pos < len(s.source) && s.source[s.pos] == '.' {
			s.pos++
			continue
		}
		return parts, true
	}
}

func (s *tomlScanner) keyPart() (string, bool) {
	if s.pos >= len(s.source) {
		return "", false
	}
	if s.source[s.pos] == '"' || s.source[s.pos] == '\'' {
		return s.stringValue()
	}
	start := s.pos
	for s.pos < len(s.source) && isBareKeyByte(s.source[s.pos]) {
		s.pos++
	}
	if s.pos == start {
		return "", false
	}
	return s.source[start:s.pos], true
}

func (s *tomlScanner) value(table, key string) bool {
	if s.pos >= len(s.source) {
		return false
	}
	switch s.source[s.pos] {
	case '"', '\'':
		text, ok := s.stringValue()
		if !ok {
			return false
		}
		s.set(table, key, text)
		return true
	case '{':
		return s.inlineTable(joinTablePath(table, []string{key}))
	case '[':
		// Arrays never carry a service tier; step over them as one opaque unit.
		return s.skipArray()
	}
	start := s.pos
	for s.pos < len(s.source) {
		switch s.source[s.pos] {
		case '\n', '#', ',', '}':
			s.set(table, key, strings.TrimSpace(s.source[start:s.pos]))
			return true
		}
		s.pos++
	}
	s.set(table, key, strings.TrimSpace(s.source[start:s.pos]))
	return true
}

func (s *tomlScanner) inlineTable(table string) bool {
	s.pos++ // '{'
	for {
		s.skipIgnorable()
		if s.pos >= len(s.source) {
			return false
		}
		switch s.source[s.pos] {
		case '}':
			s.pos++
			return true
		case ',':
			s.pos++
			continue
		}
		if !s.keyValue(table) {
			return false
		}
	}
}

func (s *tomlScanner) skipArray() bool {
	depth := 0
	for s.pos < len(s.source) {
		switch s.source[s.pos] {
		case '[':
			depth++
			s.pos++
		case ']':
			depth--
			s.pos++
			if depth == 0 {
				return true
			}
		case '"', '\'':
			if _, ok := s.stringValue(); !ok {
				return false
			}
		case '#':
			for s.pos < len(s.source) && s.source[s.pos] != '\n' {
				s.pos++
			}
		default:
			s.pos++
		}
	}
	return false
}

func (s *tomlScanner) stringValue() (string, bool) {
	quote := s.source[s.pos]
	triple := string([]byte{quote, quote, quote})
	if strings.HasPrefix(s.source[s.pos:], triple) {
		return s.multilineString(triple, quote == '"')
	}
	basic := quote == '"'
	s.pos++
	var out strings.Builder
	for s.pos < len(s.source) {
		char := s.source[s.pos]
		switch {
		case char == '\n':
			return "", false // a single-line string may not span lines
		case basic && char == '\\':
			if !s.escape(&out) {
				return "", false
			}
		case char == quote:
			s.pos++
			return out.String(), true
		default:
			out.WriteByte(char)
			s.pos++
		}
	}
	return "", false
}

func (s *tomlScanner) multilineString(terminator string, basic bool) (string, bool) {
	s.pos += len(terminator)
	if strings.HasPrefix(s.source[s.pos:], "\r\n") {
		s.pos += 2
	} else if strings.HasPrefix(s.source[s.pos:], "\n") {
		s.pos++
	}
	var out strings.Builder
	for s.pos < len(s.source) {
		if strings.HasPrefix(s.source[s.pos:], terminator) {
			s.pos += len(terminator)
			return out.String(), true
		}
		if basic && s.source[s.pos] == '\\' {
			if s.pos+1 < len(s.source) && (s.source[s.pos+1] == '\n' || s.source[s.pos+1] == '\r') {
				s.pos++ // line-ending backslash trims the following whitespace
				for s.pos < len(s.source) && (s.source[s.pos] == '\n' || s.source[s.pos] == '\r' || s.source[s.pos] == ' ' || s.source[s.pos] == '\t') {
					s.pos++
				}
				continue
			}
			if !s.escape(&out) {
				return "", false
			}
			continue
		}
		out.WriteByte(s.source[s.pos])
		s.pos++
	}
	return "", false
}

func (s *tomlScanner) escape(out *strings.Builder) bool {
	if s.pos+1 >= len(s.source) {
		return false
	}
	char := s.source[s.pos+1]
	s.pos += 2
	switch char {
	case 'n':
		out.WriteByte('\n')
	case 't':
		out.WriteByte('\t')
	case 'r':
		out.WriteByte('\r')
	case 'b':
		out.WriteByte('\b')
	case 'f':
		out.WriteByte('\f')
	case '"', '\\', '\'':
		out.WriteByte(char)
	case 'u', 'U':
		width := 4
		if char == 'U' {
			width = 8
		}
		if s.pos+width > len(s.source) {
			return false
		}
		code, err := strconv.ParseUint(s.source[s.pos:s.pos+width], 16, 32)
		if err != nil {
			return false
		}
		s.pos += width
		if !utf8.ValidRune(rune(code)) {
			return false
		}
		out.WriteRune(rune(code))
	default:
		return false
	}
	return true
}

func (s *tomlScanner) set(table, key, value string) {
	values, ok := s.document[table]
	if !ok {
		values = map[string]string{}
		s.document[table] = values
	}
	values[key] = value
}

func joinTablePath(table string, parts []string) string {
	joined := strings.Join(parts, ".")
	if table == "" {
		return joined
	}
	return table + "." + joined
}

func isBareKeyByte(char byte) bool {
	return char == '_' || char == '-' ||
		(char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9')
}

// codexConfigTier reports the service tier a Codex config.toml applies by
// default: the selected profile's tier when the file selects one, otherwise
// the root tier. A tier written under an unselected [profiles.*] table belongs
// to that profile alone and must not price the rest of the ledger.
func codexConfigTier(configDir string) string {
	encoded, err := readCodexConfig(configDir)
	if err != nil {
		return ""
	}
	document := parseTOML(encoded)
	if profile, ok := document.value("", "profile"); ok {
		return codexProfileTier(document, profile)
	}
	tier, _ := document.value("", "service_tier")
	return tier
}

// codexProfileTier reads one named profile's tier, falling back to the root
// tier the profile inherits when the profile itself is silent.
func codexProfileTier(document tomlDocument, profile string) string {
	profile = strings.TrimSpace(profile)
	if profile != "" {
		if tier, ok := document.value("profiles."+profile, "service_tier"); ok {
			return tier
		}
	}
	tier, _ := document.value("", "service_tier")
	return tier
}

func codexConfigProfileTier(configDir, profile string) string {
	encoded, err := readCodexConfig(configDir)
	if err != nil {
		return ""
	}
	return codexProfileTier(parseTOML(encoded), profile)
}

// readCodexConfig refuses an empty directory rather than resolving
// "config.toml" against whatever the process happens to be running in.
func readCodexConfig(configDir string) (string, error) {
	if strings.TrimSpace(configDir) == "" {
		return "", os.ErrNotExist
	}
	encoded, err := os.ReadFile(filepath.Join(configDir, "config.toml"))
	return string(encoded), err
}

// fastServiceTier maps a Codex service tier onto the premium pricing multiplier.
func fastServiceTier(tier string) bool {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "fast", "priority":
		return true
	default:
		return false
	}
}
