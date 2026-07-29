package mirror

import (
	"strings"
	"testing"
)

func TestReflowANSIMatchesTypeScriptGoldenCases(t *testing.T) {
	tests := []struct {
		name  string
		input string
		width int
		want  string
	}{
		{
			name:  "short line is padded",
			input: "hello world",
			width: 80,
			want:  "hello world" + strings.Repeat(" ", 69),
		},
		{
			name:  "prose wraps at spaces",
			input: "one two three four five six seven",
			width: 12,
			want:  "one two     \r\nthree four  \r\nfive six    \r\nseven       ",
		},
		{
			name:  "indent survives continuation",
			input: "  - this is a long bullet that needs to wrap onto multiple lines",
			width: 20,
			want:  "  - this is a long  \r\n  bullet that needs \r\n  to wrap onto      \r\n  multiple lines    ",
		},
		{
			name:  "cursor forward expands",
			input: "A\x1b[5CB",
			width: 80,
			want:  "A     B" + strings.Repeat(" ", 73),
		},
		{
			name:  "wide characters",
			input: "界面🙂 alpha beta gamma",
			width: 10,
			want:  "界面🙂    \r\nalpha     \r\nbeta gamma",
		},
		{
			name:  "SGR carries across breaks",
			input: "\x1b[31mthis is a long red sentence that wraps\x1b[0m",
			width: 15,
			want:  "\x1b[31mthis is a long \x1b[0m\r\n\x1b[31mred sentence   \x1b[0m\r\n\x1b[31mthat wraps     \x1b[0m",
		},
		{
			name:  "multi-space columns are ordinary prose in current TS",
			input: "a    b    c    d",
			width: 5,
			want:  "a    \r\nb    \r\nc    \r\nd    ",
		},
		{
			name:  "pipe table is preserved",
			input: "| Brand | Format | Size |",
			width: 10,
			want:  "| Brand | Format | Size |",
		},
		{
			name:  "box drawing is preserved",
			input: "╭────────────────────╮",
			width: 10,
			want:  "╭────────────────────╮",
		},
		{
			name:  "hard newlines and empty rows survive",
			input: "first\r\n\r\nthird",
			width: 80,
			want:  "first" + strings.Repeat(" ", 75) + "\r\n\r\nthird" + strings.Repeat(" ", 75),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ReflowANSI(test.input, test.width); got != test.want {
				t.Fatalf("ReflowANSI() = %q\nwant %q", got, test.want)
			}
		})
	}
}

func TestReflowANSIFloorsTinyWidthsAndHardBreaks(t *testing.T) {
	got := ReflowANSI("abcdefghij", 1)
	if want := "abcd\r\nefgh\r\nij  "; got != want {
		t.Fatalf("ReflowANSI() = %q, want %q", got, want)
	}
}

func TestReflowToUsesSerializedViewport(t *testing.T) {
	m := newTestMirror(t, 40, 4)
	writeString(t, m, "this is a sentence that is wider than twelve columns")
	got := m.ReflowTo(12)
	if !strings.Contains(got, "\r\n") {
		t.Fatalf("ReflowTo() did not wrap: %q", got)
	}
	for _, line := range strings.Split(got, "\r\n") {
		if visibleWidth(stripANSI(line)) > 12 {
			t.Fatalf("line exceeds width 12: %q", line)
		}
	}
}

func TestReflowToIncludesMainBufferWhenAlternateIsActive(t *testing.T) {
	m := newTestMirror(t, 40, 4)
	writeString(t, m, "main\x1b[?1049h\x1b[2J\x1b[Halt")
	got := stripANSI(m.ReflowTo(40))
	if !strings.Contains(got, "mainalt") {
		t.Fatalf("ReflowTo() = %q, want serialized main and alternate buffers", got)
	}
}
