package api

import (
	"fmt"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// Resuming a conversation that has already been continued once.
//
// From the owner's recoveries on 11 September: `sessions resume <source>` was
// refused because the source was linked to a successor — and that successor had
// itself failed (exit 1, socket timeout). Resuming the failed successor worked.
// So the refusal was standing between him and the one thing that would have
// worked, while naming an id he then had to resume by hand.
//
// A person resuming a conversation means the conversation, not the record. The
// chain of successors is followed to its newest link, and:
//
//   - if that link is still running, resuming is refused, because starting a
//     second runner on a live conversation is how two agents end up writing
//     into one transcript. The refusal says which session to open;
//   - otherwise that link is the conversation being asked for, and it is what
//     gets resumed. Continuing from an ancestor instead would fork the
//     conversation and silently drop everything the successor did.

// resumeTarget is where a resume should actually start from.
type resumeTarget struct {
	// Session is the record to resume: the source itself when it has no
	// successor, or the newest link in its chain.
	Session state.SessionInfo
	// Followed is the chain that was walked, oldest first, when one existed.
	Followed []string
	// Refusal is set when nothing should be started. It says what to do.
	Refusal string
}

// resolveResumeTarget walks the ReopenedAs chain from a source record.
//
// repairLaneID is the successor a repair is already in the middle of creating;
// finding it is not a conflict with itself.
func resolveResumeTarget(sessions []state.SessionInfo, source state.SessionInfo, repairLaneID string) resumeTarget {
	byID := make(map[string]state.SessionInfo, len(sessions))
	for _, candidate := range sessions {
		byID[candidate.ID] = candidate
	}
	current := source
	followed := []string{}
	seen := map[string]struct{}{current.ID: {}}
	for current.ReopenedAs != "" && current.ReopenedAs != repairLaneID {
		next, ok := byID[current.ReopenedAs]
		if !ok {
			// The successor is not in the listing: it was archived, or its
			// record is gone. Naming it is the most this can honestly do.
			return resumeTarget{Session: source, Followed: followed, Refusal: fmt.Sprintf(
				"%s was already continued as %s, which is no longer listed; resume %s directly, or use --force to continue from this record and fork the conversation",
				shortID(source.ID), shortID(current.ReopenedAs), shortID(current.ReopenedAs),
			)}
		}
		if _, loop := seen[next.ID]; loop {
			break
		}
		seen[next.ID] = struct{}{}
		followed = append(followed, next.ID)
		current = next
	}
	if len(followed) == 0 {
		return resumeTarget{Session: source}
	}
	if sessionStillRunning(current) {
		return resumeTarget{Session: current, Followed: followed, Refusal: fmt.Sprintf(
			"%s was already continued%s, and %s is running now; open it instead of starting a second runner on the same conversation",
			shortID(source.ID), describeChain(followed[:len(followed)-1]), shortID(current.ID),
		)}
	}
	return resumeTarget{Session: current, Followed: followed}
}

// sessionStillRunning is the one question that decides between resuming and
// refusing. A session whose runner is gone or unreachable is not running: that
// is exactly the session the owner was trying to bring back.
func sessionStillRunning(session state.SessionInfo) bool {
	return !session.Exited && !session.RunnerGone && !session.Unreachable
}

func describeChain(intermediate []string) string {
	if len(intermediate) == 0 {
		return ""
	}
	names := make([]string, 0, len(intermediate))
	for _, id := range intermediate {
		names = append(names, shortID(id))
	}
	return " as " + strings.Join(names, ", then ")
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
