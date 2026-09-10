package localnetwork

import (
	"errors"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestExplainAnnotatesDarwinLocalNetworkFailures(t *testing.T) {
	tests := []struct {
		name        string
		endpoint    string
		err         error
		annotated   bool
		originalErr string
	}{
		{"private-ip", "http://10.129.174.32:8787", errors.New("dial tcp 10.129.174.32:8787: connect: no route to host"), true, "dial tcp 10.129.174.32:8787: connect: no route to host"},
		{"private-wrapped-errno", "http://192.168.1.20:8787", syscall.EHOSTUNREACH, true, "no route to host"},
		{"link-local", "http://169.254.2.4:8787", syscall.EHOSTUNREACH, true, "no route to host"},
		{"tailnet", "http://100.92.1.2:8787", syscall.EHOSTUNREACH, false, "no route to host"},
		{"public", "https://example.com", syscall.EHOSTUNREACH, false, "no route to host"},
		{"other-error", "http://10.0.0.2:8787", syscall.ECONNREFUSED, false, "connection refused"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.err
			// A plain text fixture pins the real observed string while the wrapped
			// errno fixture proves classification. Go cannot recover an errno from
			// an errors.New string, so only Darwin's wrapped cases are rewritten.
			if test.name == "private-ip" {
				err = &fixtureError{text: test.err.Error(), cause: syscall.EHOSTUNREACH}
			}
			got := Explain(test.endpoint, err)
			wantAnnotated := test.annotated && runtime.GOOS == "darwin"
			if IsPossiblePermissionError(got) != wantAnnotated {
				t.Fatalf("Explain(%q, %q) = %q, annotated=%v want %v", test.endpoint, err, got, !wantAnnotated, wantAnnotated)
			}
			// The transport failure the operating system actually reported has to
			// survive: the message keeps it, and errno matching keeps working.
			if !strings.Contains(got.Error(), test.originalErr) {
				t.Fatalf("Explain(%q, %q) = %q, want it to preserve %q", test.endpoint, err, got, test.originalErr)
			}
			if errors.Is(err, syscall.EHOSTUNREACH) != errors.Is(got, syscall.EHOSTUNREACH) {
				t.Fatalf("Explain(%q, %q) = %q lost the underlying errno", test.endpoint, err, got)
			}
			if !wantAnnotated && got.Error() != err.Error() {
				t.Fatalf("Explain(%q, %q) = %q, want the original error unchanged", test.endpoint, err, got)
			}
		})
	}
}

// A permission-shaped failure must never be mistaken for proof of denial by a
// caller that only has the error value.
func TestPossiblePermissionErrorIsNotAssertedForOtherFailures(t *testing.T) {
	if IsPossiblePermissionError(nil) || IsPossiblePermissionError(errors.New(PossibleCause+" (copied text)")) {
		t.Fatal("possible-permission classification must come from Explain, not from message text")
	}
}

type fixtureError struct {
	text  string
	cause error
}

func (e *fixtureError) Error() string { return e.text }
func (e *fixtureError) Unwrap() error { return e.cause }
