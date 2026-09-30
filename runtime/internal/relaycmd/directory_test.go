package relaycmd

import (
	"errors"
	"io"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/relay"
)

func TestDirectoryFlagsRemainRecognizedButRefuseBeforeCredentialRead(t *testing.T) {
	for _, extra := range [][]string{nil, {"--allow-file", "/fixture/allow-list"}} {
		args := append([]string{"--directory-url", "https://fixture.invalid", "--owner-token-file", "/fixture/nonexistent"}, extra...)
		config, help, err := parse(args, io.Discard)
		if err != nil || help {
			t.Fatalf("legacy flags not recognized: help=%t, error=%v", help, err)
		}
		if _, err := authorizer(config); !errors.Is(err, relay.ErrDirectoryAuthorizationUnavailable) {
			t.Fatalf("unsupported configuration = %v, want unavailable without reading token", err)
		}
	}
	if _, err := authorizer(Config{AllowFile: "/fixture/allow-list"}); err != nil {
		t.Fatalf("static allow-list configuration refused: %v", err)
	}
}
