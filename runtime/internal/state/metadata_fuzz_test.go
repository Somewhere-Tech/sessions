package state

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func FuzzRunnerMetadataParse(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		first, firstErr := parseRunnerMetadata(data)
		second, secondErr := parseRunnerMetadata(data)
		if (firstErr == nil) != (secondErr == nil) ||
			(firstErr != nil && firstErr.Error() != secondErr.Error()) ||
			!reflect.DeepEqual(first, second) {
			t.Fatalf("metadata parse is nondeterministic: first=%#v/%v second=%#v/%v", first, firstErr, second, secondErr)
		}
		if !json.Valid(data) && firstErr == nil {
			t.Fatalf("invalid JSON parsed successfully: %q", data)
		}
		if firstErr == nil && strings.TrimSpace(first.Info.ID) == "" {
			t.Fatalf("metadata without an id parsed successfully: %#v", first)
		}
	})
}
