//go:build linux

package resource

import (
	"testing"
	"time"
)

// The comm field is the trap in /proc/<pid>/stat: it is parenthesised, and it
// can contain spaces and parentheses of its own, so any parser that splits the
// whole line on whitespace mis-reads every field after it. These are real
// shapes -- a Chrome helper and a process whose name contains ") (" -- not
// invented ones.
func TestParseProcStatHandlesAwkwardCommFields(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		want  Process
		valid bool
	}{
		{
			name:  "plain",
			line:  "1234 (claude) S 1200 1234 1234 0 -1 4194304 100 0 0 0 250 125 0 0 20 0 12 0 900 0 0",
			want:  Process{PID: 1234, PPID: 1200, Name: "claude", CPUTime: 3750 * time.Millisecond},
			valid: true,
		},
		{
			name:  "comm with spaces and parens",
			line:  "77 (Web Content (1)) S 42 77 77 0 -1 0 0 0 0 0 100 0 0 0 20 0 3 0 5 0 0",
			want:  Process{PID: 77, PPID: 42, Name: "Web Content (1)", CPUTime: time.Second},
			valid: true,
		},
		{
			name: "truncated line",
			line: "5 (short) S 1 5",
		},
		{
			name: "no comm",
			line: "garbage",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := parseProcStat(test.line)
			if ok != test.valid {
				t.Fatalf("ok = %v, want %v", ok, test.valid)
			}
			if !test.valid {
				return
			}
			if got.PID != test.want.PID || got.PPID != test.want.PPID ||
				got.Name != test.want.Name || got.CPUTime != test.want.CPUTime {
				t.Fatalf("got %+v, want %+v", got, test.want)
			}
		})
	}
}
