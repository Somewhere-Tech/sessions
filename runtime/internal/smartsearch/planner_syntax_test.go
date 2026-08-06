package smartsearch

import (
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/search"
)

// The planner prompt asks for FTS5: OR between synonyms, near() for
// proximity, quotes for a remembered phrase. Ordinary queries are parsed
// conjunctively and drop a bare OR as a stopword, so an unmarked plan would
// run meaning every synonym must appear -- the inverse of the recall the
// planner exists to produce.
func TestPlannedQueryIsMarkedAsRawSyntax(t *testing.T) {
	for _, planned := range []string{
		`{"query":"auth OR login OR credentials"}`,
		`{"query":"near(deploy,rollback,5)"}`,
		`{"query":"\"exact remembered phrase\""}`,
	} {
		plan, err := decodePlan(planned)
		if err != nil {
			t.Fatalf("decodePlan(%s): %v", planned, err)
		}
		if !strings.HasPrefix(plan.Query, search.RawSyntaxPrefix) {
			t.Fatalf("planned query %q is not marked raw; a bare OR would be dropped as a stopword",
				plan.Query)
		}
	}
}

// A planner that already marks its own output must not be marked twice.
func TestAnAlreadyMarkedPlanIsLeftAlone(t *testing.T) {
	plan, err := decodePlan(`{"query":"fts:auth OR login"}`)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Query != "fts:auth OR login" {
		t.Fatalf("query = %q, want it untouched", plan.Query)
	}
}
