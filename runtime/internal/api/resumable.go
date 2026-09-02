package api

import (
	"sort"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

// resumableListing is the Resume view plus the degradation of the history it
// was built from. A conversation whose transcript could not be read is dropped
// from or thinned in the merge below, so without these counters a Resume list
// that is missing conversations looks exactly like one where those
// conversations never existed.
type resumableListing struct {
	Sessions           []watch.ResumableSession `json:"sessions"`
	UnreadableSessions int                      `json:"unreadable_sessions,omitempty"`
	SkippedRecords     int                      `json:"skipped_records,omitempty"`
}

// resumableConversations projects provider files and durable Sessions history
// into one row per provider conversation. The provider UUID is the identity;
// individual Sessions runtimes are continuation-chain evidence, not duplicate
// conversations.
func (s *Server) resumableConversations() resumableListing {
	// The Resume view shows titles, folders, and recency, never message
	// counts, so it takes the cheap listing. The exact listing parses every
	// transcript whose file changed, and a live conversation changes every
	// few seconds; on a real corpus that made each open of the Resume dialog
	// re-read gigabytes and take ten seconds, while this listing takes tens of
	// milliseconds once its per-file cache is warm. Rows degrade one at a
	// time and the error is nil unconditionally.
	sessions, _ := s.integrationEndpoints.SearchSessions(s.registry.List(true))
	unreadable, skipped := 0, 0
	for _, session := range sessions {
		if session.Unreadable {
			unreadable++
		}
		skipped += session.SkippedRecords
	}
	// The listing has already populated the provider scan cache. Reuse it so
	// the Resume dialog does not wait for a second full filesystem traversal.
	scanned := s.integrationEndpoints.ResumableProviderConversations()
	return resumableListing{
		Sessions:           mergeResumableConversations(scanned, sessions),
		UnreadableSessions: unreadable,
		SkippedRecords:     skipped,
	}
}

func mergeResumableConversations(
	scanned []watch.ResumableSession,
	history []integrations.HistorySession,
) []watch.ResumableSession {
	byIdentity := make(map[string]*watch.ResumableSession, len(scanned))
	for index := range scanned {
		session := scanned[index]
		// Provider scans see Claude and Codex history regardless of whether a
		// conversation was opened through Sessions. A linked durable Sessions
		// runtime below clears this marker.
		session.External = true
		key := resumableIdentity(session.Tool, session.SessionID)
		copy := session
		byIdentity[key] = &copy
	}

	runSeen := make(map[string]map[string]struct{}, len(byIdentity))
	for _, source := range history {
		tool := resumableTool(source.Tool)
		if tool == "" {
			continue
		}
		if strings.TrimSpace(source.ProviderSessionID) == "" {
			if !source.ConversationAvailable || source.PromptHistoryOnly {
				continue
			}
			key := "sessions-history:" + tool + ":" + source.ID
			if _, exists := byIdentity[key]; exists {
				continue
			}
			byIdentity[key] = &watch.ResumableSession{
				SessionID: source.ID, Tool: tool, Origin: "Sessions recovery",
				Title: source.Name, HistoryID: source.ID, Cwd: source.CWD,
				ModifiedAt: float64(source.LastActivityAt), FirstUserMessage: source.Name,
				TranscriptRecovery: true,
				Runs: []watch.ResumableRun{{
					SessionID: source.ID, Name: source.Name, StartedAt: source.CreatedAt,
					LastActivityAt: source.LastActivityAt, Machine: source.Machine,
					CreatorKind: source.CreatorKind,
					ReopenedAs:  source.ReopenedAs, ResumedFrom: source.ResumedFrom,
				}},
			}
			continue
		}
		key := resumableIdentity(tool, source.ProviderSessionID)
		session := byIdentity[key]
		if session == nil && !source.PromptHistoryOnly {
			// The provider's own file is not on this machine (pruned, moved, or
			// written under another provider home), but Sessions kept its own
			// copy and can resume from it. Dropping the row here made a
			// conversation Sessions ran disappear from Resume while History
			// still listed it.
			if !source.ConversationAvailable || source.External {
				continue
			}
			session = &watch.ResumableSession{
				SessionID: source.ProviderSessionID, Tool: tool, Origin: "Sessions copy",
				Title: source.Name, HistoryID: source.ID, Cwd: source.CWD,
				ModifiedAt: float64(source.LastActivityAt), FirstUserMessage: source.Name,
				TranscriptRecovery: true,
			}
			byIdentity[key] = session
		}
		if session == nil {
			session = &watch.ResumableSession{
				SessionID: source.ProviderSessionID, Tool: tool,
				Origin: "Claude prompt index", Title: source.Name,
				Cwd: source.CWD, ModifiedAt: float64(source.LastActivityAt),
				FirstUserMessage: source.Name, PromptHistoryOnly: true,
				External: source.External,
			}
			byIdentity[key] = session
		}
		if source.PromptHistoryOnly {
			session.HistoryID = source.ID
			session.PromptHistoryOnly = true
			session.Origin = "Claude prompt index"
		} else if session.HistoryID == "" {
			session.HistoryID = source.ID
		}
		if preferredHistoryTitle(session.Title, source.Name) {
			session.Title = source.Name
		}
		if source.CWD != "" && session.Cwd == "" {
			session.Cwd = source.CWD
		}
		if float64(source.LastActivityAt) > session.ModifiedAt {
			session.ModifiedAt = float64(source.LastActivityAt)
		}
		if source.External {
			continue
		}
		session.External = false
		seen := runSeen[key]
		if seen == nil {
			seen = make(map[string]struct{})
			runSeen[key] = seen
		}
		if _, exists := seen[source.ID]; exists {
			continue
		}
		seen[source.ID] = struct{}{}
		session.Runs = append(session.Runs, watch.ResumableRun{
			SessionID: source.ID, Name: source.Name, StartedAt: source.CreatedAt,
			LastActivityAt: source.LastActivityAt, Machine: source.Machine,
			CreatorKind: source.CreatorKind,
			ReopenedAs:  source.ReopenedAs, ResumedFrom: source.ResumedFrom,
			MovedToEndpoint: source.MovedToEndpoint, MovedToSessionID: source.MovedToSessionID,
			MovedFromEndpoint: source.MovedFromEndpoint, MovedFromSessionID: source.MovedFromSessionID,
		})
	}

	result := make([]watch.ResumableSession, 0, len(byIdentity))
	for _, session := range byIdentity {
		sort.SliceStable(session.Runs, func(i, j int) bool {
			if session.Runs[i].StartedAt != session.Runs[j].StartedAt {
				return session.Runs[i].StartedAt < session.Runs[j].StartedAt
			}
			return session.Runs[i].SessionID < session.Runs[j].SessionID
		})
		result = append(result, *session)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].ModifiedAt != result[j].ModifiedAt {
			return result[i].ModifiedAt > result[j].ModifiedAt
		}
		return resumableIdentity(result[i].Tool, result[i].SessionID) <
			resumableIdentity(result[j].Tool, result[j].SessionID)
	})
	return result
}

func resumableTool(value string) string {
	switch strings.TrimSpace(value) {
	case "claude", "claude-code":
		return "claude"
	case "codex":
		return "codex"
	default:
		return ""
	}
}

func resumableIdentity(tool, providerID string) string {
	return strings.TrimSpace(tool) + ":" + strings.TrimSpace(providerID)
}

func preferredHistoryTitle(current, candidate string) bool {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return false
	}
	current = strings.TrimSpace(current)
	return current == "" ||
		strings.HasPrefix(strings.ToLower(current), "claude — ") ||
		strings.HasPrefix(strings.ToLower(current), "codex — ")
}
