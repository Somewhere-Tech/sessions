package recovery

import (
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

// The decoder is the inverse of watch.EncodeClaudeCWD. When it only knew the
// Unix rule, a Windows transcript directory "C--Users-x-proj" decoded to
// "/C/Users/x/proj" and adoption resolved a cwd that exists on no machine.
func TestDecodeClaudeProjectDirName(t *testing.T) {
	tests := []struct {
		encoded string
		want    string
	}{
		{encoded: "-Users-example-project", want: "/Users/example/project"},
		{encoded: "Users-example-project", want: "/Users/example/project"},
		{encoded: `C--Users-example-project`, want: `C:\Users\example\project`},
		{encoded: `d--work-repo`, want: `d:\work\repo`},
		{encoded: "C--", want: `C:\`},
		{encoded: "", want: ""},
	}
	for _, test := range tests {
		if got := decodeClaudeProjectDirName(test.encoded); got != test.want {
			t.Fatalf("decodeClaudeProjectDirName(%q) = %q, want %q", test.encoded, got, test.want)
		}
	}
}

// Round-tripping through the real encoder is the property that matters: any
// path whose components carry no dash must survive encode-then-decode.
func TestDecodeClaudeProjectDirNameInvertsTheEncoder(t *testing.T) {
	for _, cwd := range []string{
		"/Users/example/project",
		"/var/tmp/work",
		`C:\Users\example\project`,
		`D:\work\repo\nested`,
	} {
		encoded := watch.EncodeClaudeCWD(cwd)
		if got := decodeClaudeProjectDirName(encoded); got != cwd {
			t.Fatalf("decode(encode(%q)) = %q via %q", cwd, got, encoded)
		}
	}
}
