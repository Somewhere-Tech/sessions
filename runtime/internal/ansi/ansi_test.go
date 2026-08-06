package ansi

import "testing"

// TestStripUnion pins the union of the four implementations this replaced.
// Every row was handled by at least one of them and missed by at least one
// other, which is how the same escape could disappear from a transcript and
// survive in a snapshot of the same session.
func TestStripUnion(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "sgr", in: "\x1b[1;31mred\x1b[0m", want: "red"},
		{name: "csi with intermediate byte", in: "a\x1b[1 qb", want: "ab"},
		{name: "csi private parameter", in: "\x1b[?25lhidden\x1b[?25h", want: "hidden"},
		{name: "csi non-alpha final", in: "x\x1b[1@y", want: "xy"},
		{name: "osc terminated by bel", in: "\x1b]0;title\x07body", want: "body"},
		{name: "osc terminated by st", in: "\x1b]0;title\x1b\\body", want: "body"},
		{name: "charset designator", in: "\x1b(Bplain", want: "plain"},
		{name: "dcs string", in: "\x1bPqsome\ndata\x1b\\kept", want: "kept"},
		{name: "apc string", in: "\x1b_payload\x1b\\kept", want: "kept"},
		{name: "stray two byte escape", in: "a\x1bMb", want: "ab"},
		{name: "layout survives", in: "one\ttwo\r\nthree", want: "one\ttwo\r\nthree"},
		{name: "bare control byte", in: "a\x07b", want: "ab"},
		{name: "plain text untouched", in: "nothing to strip", want: "nothing to strip"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Strip(test.in); got != test.want {
				t.Fatalf("Strip(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

// TestStripRemovesOSCBeforeTheGenericEscapeRule guards the ordering: applying
// the two-byte escape rule first eats the ESC ] introducer and leaves the
// string body as visible text.
func TestStripRemovesOSCBeforeTheGenericEscapeRule(t *testing.T) {
	if got := Strip("\x1b]777;notify;title;body\x07after"); got != "after" {
		t.Fatalf("Strip() = %q, want %q", got, "after")
	}
}
