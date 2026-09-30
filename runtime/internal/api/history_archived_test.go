package api

import (
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
)

// Archiving a session takes it out of the list and leaves the conversation
// where it was. History therefore keeps offering it, and has to say which it
// is — a row that reads like every other one is how a person loses track of
// what they already put away.
func TestMarkArchivedNamesOnlyTheArchivedRows(t *testing.T) {
	sessions := []integrations.HistorySession{
		{ID: "put-away"}, {ID: "still-listed"},
	}

	markArchived(sessions, []string{"put-away", "never-listed-here"})

	if !sessions[0].Archived {
		t.Fatal("the archived conversation is not marked, so History cannot say so")
	}
	if sessions[1].Archived {
		t.Fatal("a session nobody archived was marked archived")
	}
}

// A runtime that cannot answer marks nothing. An unmarked row is what every
// client already renders; a guessed one would be a claim about somebody's
// history that nothing checked.
func TestMarkArchivedLeavesRowsAloneWhenNothingIsArchived(t *testing.T) {
	sessions := []integrations.HistorySession{{ID: "one", Archived: false}}

	markArchived(sessions, nil)

	if sessions[0].Archived {
		t.Fatal("a row was marked archived with no archived ids to go on")
	}
}
