package main

import (
	"reflect"
	"testing"
)

func TestCLIYOLODefaultPreservesJoinedPermissionFlags(t *testing.T) {
	for _, test := range []struct{ command, argument string }{
		{"claude", "--permission-mode=plan"},
		{"codex", "--sandbox=read-only"},
		{"codex", "--ask-for-approval=untrusted"},
	} {
		t.Run(test.argument, func(t *testing.T) {
			body := createSessionRequest{Cmd: test.command, Args: []string{test.argument}}
			if err := applyToolDefault(&body, true); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(body.Args, []string{test.argument}) {
				t.Fatalf("explicit policy changed: %v", body.Args)
			}
		})
	}
}
