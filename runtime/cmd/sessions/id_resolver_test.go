package main

import (
	"strings"
	"testing"
)

func TestSharedIDResolverListsEveryAmbiguousCandidate(t *testing.T) {
	candidates := []idCandidate{
		labeledID("534c15f6-1111-4111-8111-111111111111", "phone nearby"),
		labeledID("534c15f6-2222-4222-8222-222222222222", "laptop tailnet"),
	}
	_, found, err := resolveIDPrefix("534c15f6", "access request", "sessions access requests", candidates)
	if err == nil || found {
		t.Fatalf("ambiguous prefix = found %v, err %v", found, err)
	}
	message := err.Error()
	for _, want := range []string{
		"ambiguous access request prefix \"534c15f6\"; candidates:",
		candidates[0].id, candidates[1].id, "sessions access requests",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("ambiguity sentence %q does not contain %q", message, want)
		}
	}
}

func TestSharedIDResolverPrefersAnExactIDOverItsPrefixMatches(t *testing.T) {
	candidates := []idCandidate{
		labeledID("machine-one", "exact"),
		labeledID("machine-one-longer", "prefix match"),
	}
	id, found, err := resolveIDPrefix("machine-one", "machine", "sessions machines", candidates)
	if err != nil || !found || id != "machine-one" {
		t.Fatalf("exact resolution = %q, %v, %v", id, found, err)
	}
}
