package ledger

import "testing"

func lostLaneWithResumeArgv(id string) LaneState {
	return LaneState{
		LaneID:       id,
		Created:      true,
		RunnerReady:  true,
		Tool:         "claude",
		Cwd:          "/tmp/work",
		ProviderUUID: "0d7f4c1e-3b2a-4c5d-8e9f-1a2b3c4d5e6f",
		ResumeArgv:   []string{"claude", "--resume", "0d7f4c1e-3b2a-4c5d-8e9f-1a2b3c4d5e6f"},
	}
}

// A conversation whose provider transcript was pruned but which Sessions kept
// its own copy of is recoverable. Presenting it as blocked is the failure the
// mirror exists to prevent -- the user can see the session and cannot get the
// work back.
func TestMirroredConversationIsRecoverableRatherThanBlocked(t *testing.T) {
	lane := lostLaneWithResumeArgv("lane-mirrored")
	classification := ClassifyLane(lane, RuntimeState{
		ResumeSourceKnown:      true,
		ResumeSourceExists:     false,
		TranscriptMirrorUsable: true,
	})

	// The anomaly still stands: the provider file really is missing, and a
	// caller that hands `claude --resume` this id will still be refused.
	if !HasAnomaly(classification, AnomalyResumeSourceMissing) {
		t.Fatal("a mirror cleared the missing-provider-source anomaly; it must not")
	}

	plan := BuildRecoveryPlan([]Classification{classification})
	if len(plan.Recipes) != 1 {
		t.Fatalf("plan has %d recipes, want 1", len(plan.Recipes))
	}
	recipe := plan.Recipes[0]
	if recipe.Blocked {
		t.Fatal("a conversation Sessions still holds a copy of was reported as unrecoverable")
	}
	if !recipe.TranscriptRecovery {
		t.Fatal("the recipe did not say it must be recovered from the transcript copy")
	}
}

// Without a copy, nothing changed: the conversation really is gone and saying
// otherwise would send the caller at a resume the provider will refuse.
func TestConversationWithNoCopyStaysBlocked(t *testing.T) {
	classification := ClassifyLane(lostLaneWithResumeArgv("lane-gone"), RuntimeState{
		ResumeSourceKnown:  true,
		ResumeSourceExists: false,
	})
	plan := BuildRecoveryPlan([]Classification{classification})
	if len(plan.Recipes) != 1 {
		t.Fatalf("plan has %d recipes, want 1", len(plan.Recipes))
	}
	if !plan.Recipes[0].Blocked {
		t.Fatal("a conversation with neither a provider file nor a copy was offered for recovery")
	}
	if plan.Recipes[0].TranscriptRecovery {
		t.Fatal("a recipe with no copy claimed transcript recovery")
	}
}

// When the provider still has the conversation, the ordinary native resume is
// the right answer and nothing is flagged.
func TestIntactProviderSourceNeedsNoTranscriptRecovery(t *testing.T) {
	classification := ClassifyLane(lostLaneWithResumeArgv("lane-intact"), RuntimeState{
		ResumeSourceKnown:      true,
		ResumeSourceExists:     true,
		TranscriptMirrorUsable: true,
	})
	plan := BuildRecoveryPlan([]Classification{classification})
	if len(plan.Recipes) != 1 {
		t.Fatalf("plan has %d recipes, want 1", len(plan.Recipes))
	}
	if plan.Recipes[0].Blocked || plan.Recipes[0].TranscriptRecovery {
		t.Fatalf("recipe = %+v, want an ordinary unblocked native resume", plan.Recipes[0])
	}
}
