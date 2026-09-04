package localnetwork

import (
	"errors"
	"runtime"
	"syscall"
	"testing"
)

func TestExplainDarwinLocalNetworkDenials(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		err      error
		want     string
	}{
		{"private-ip", "http://10.129.174.32:8787", errors.New("dial tcp 10.129.174.32:8787: connect: no route to host"), Message},
		{"private-wrapped-errno", "http://192.168.1.20:8787", syscall.EHOSTUNREACH, Message},
		{"link-local", "http://169.254.2.4:8787", syscall.EHOSTUNREACH, Message},
		{"tailnet", "http://100.92.1.2:8787", syscall.EHOSTUNREACH, "no route to host"},
		{"public", "https://example.com", syscall.EHOSTUNREACH, "no route to host"},
		{"other-error", "http://10.0.0.2:8787", syscall.ECONNREFUSED, "connection refused"},
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
			got := Explain(test.endpoint, err).Error()
			want := test.want
			if runtime.GOOS != "darwin" && want == Message {
				want = err.Error()
			}
			if got != want {
				t.Fatalf("Explain(%q, %q) = %q, want %q", test.endpoint, err, got, want)
			}
		})
	}
}

type fixtureError struct {
	text  string
	cause error
}

func (e *fixtureError) Error() string { return e.text }
func (e *fixtureError) Unwrap() error { return e.cause }
