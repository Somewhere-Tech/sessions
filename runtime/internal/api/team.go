package api

import (
	"net/http"
	"sort"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// teamSummaryBudget bounds the summary carried on each team row. Lane-to-lane
// reads exist so a manager can see what its workers are doing without opening
// their transcripts; a manager that pulls whole conversations into its own
// context is the exact failure this endpoint is meant to prevent, so the
// summary is capped hard and the row carries no transcript, args, or env.
const teamSummaryBudget = 200

// teamMaxDepth bounds how deep the descendant walk goes. A lane is responsible
// for the work it delegated, not for an unbounded tree; a cycle in the
// parent links (which should never happen) cannot spin here.
const teamMaxDepth = 8

type teamRelation string

const (
	teamRelationSelf   teamRelation = "self"
	teamRelationParent teamRelation = "parent"
	teamRelationChild  teamRelation = "child"
)

// teamMember is the compact fact set one lane may see about another it is
// responsible for. It is deliberately small: identity, where it runs, what
// state it is in, and one short line of what it last did.
type teamMember struct {
	ID        string       `json:"id"`
	Name      string       `json:"name,omitempty"`
	Tool      string       `json:"tool"`
	Cwd       string       `json:"cwd,omitempty"`
	Relation  teamRelation `json:"relation"`
	Depth     int          `json:"depth"`
	State     string       `json:"state"`
	NeedsYou  bool         `json:"needs_you"`
	Working   bool         `json:"working"`
	Exited    bool         `json:"exited"`
	Summary   string       `json:"summary,omitempty"`
	Waiting   string       `json:"waiting,omitempty"`
	UpdatedAt int64        `json:"updated_at,omitempty"`
	// Branch and WorktreePath say where a lane's work is when it has its own
	// worktree, so a manager knows what to diff or merge without opening it.
	Branch       string `json:"branch,omitempty"`
	WorktreePath string `json:"worktree_path,omitempty"`
}

// teamListing answers "what am I responsible for". Self is the caller; parent
// is the lane that created it, if any; members are its transitive descendants.
// NeedsInput is the count of members waiting on a decision, so a manager can
// see at a glance that a worker is blocked without reading every row.
type teamListing struct {
	Self       *teamMember  `json:"self,omitempty"`
	Parent     *teamMember  `json:"parent,omitempty"`
	Members    []teamMember `json:"members"`
	NeedsInput int          `json:"needs_input"`
}

// teamState collapses the lifecycle flags into one word a caller can branch on
// without re-deriving it from working/exited/idleReason everywhere.
func teamState(info state.SessionInfo) string {
	switch {
	case info.Exited:
		return "ended"
	case info.IdleReason == state.IdleReasonNeedsInput:
		return "needs-you"
	case info.Working:
		return "working"
	case info.IdleReason == state.IdleReasonFailed:
		return "failed"
	case info.IdleReason == state.IdleReasonNeverStarted:
		return "not-started"
	default:
		return "idle"
	}
}

func teamMemberFrom(info state.SessionInfo, relation teamRelation, depth int) teamMember {
	updated := info.LastDataAt
	if info.LastAgentMessageAt != nil && *info.LastAgentMessageAt > updated {
		updated = *info.LastAgentMessageAt
	}
	member := teamMember{
		ID: info.ID, Name: info.Name, Tool: string(info.Tool), Cwd: info.Cwd,
		Relation: relation, Depth: depth, State: teamState(info),
		NeedsYou: info.IdleReason == state.IdleReasonNeedsInput,
		Working:  info.Working, Exited: info.Exited,
		Summary:   truncateBudget(info.LastSummary, teamSummaryBudget),
		Waiting:   truncateBudget(info.IdleDetail, teamSummaryBudget),
		UpdatedAt: updated,
		Branch:    info.Branch, WorktreePath: info.WorktreePath,
	}
	return member
}

func truncateBudget(value string, budget int) string {
	value = strings.TrimSpace(value)
	if len(value) <= budget {
		return value
	}
	// Trim on a rune boundary so a multibyte character is never split.
	trimmed := value[:budget]
	for len(trimmed) > 0 && !isUTF8Start(value[len(trimmed)]) {
		trimmed = trimmed[:len(trimmed)-1]
	}
	return strings.TrimSpace(trimmed) + "…"
}

func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }

// teamFor builds the listing a caller lane may see. Visibility follows
// responsibility: the caller, its parent, and its transitive descendants,
// nothing else. A caller that is not a known session gets an empty listing
// rather than the whole fleet.
func teamFor(sessions []state.SessionInfo, callerID string) (teamListing, bool) {
	byID := make(map[string]state.SessionInfo, len(sessions))
	childrenOf := make(map[string][]string, len(sessions))
	for _, info := range sessions {
		byID[info.ID] = info
		parent := effectiveParent(info)
		if parent != "" {
			childrenOf[parent] = append(childrenOf[parent], info.ID)
		}
	}
	caller, ok := byID[callerID]
	if !ok {
		return teamListing{Members: []teamMember{}}, false
	}
	listing := teamListing{Members: []teamMember{}}
	self := teamMemberFrom(caller, teamRelationSelf, 0)
	listing.Self = &self
	if parentID := effectiveParent(caller); parentID != "" {
		if parent, found := byID[parentID]; found {
			member := teamMemberFrom(parent, teamRelationParent, 0)
			listing.Parent = &member
		}
	}
	// Breadth-first over descendants, depth-capped, visited-guarded so a
	// malformed parent cycle terminates.
	visited := map[string]bool{callerID: true}
	type queued struct {
		id    string
		depth int
	}
	queue := make([]queued, 0)
	for _, child := range childrenOf[callerID] {
		queue = append(queue, queued{id: child, depth: 1})
	}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if visited[current.id] || current.depth > teamMaxDepth {
			continue
		}
		visited[current.id] = true
		info, found := byID[current.id]
		if !found {
			continue
		}
		relation := teamRelationChild
		listing.Members = append(listing.Members, teamMemberFrom(info, relation, current.depth))
		if info.IdleReason == state.IdleReasonNeedsInput && !info.Exited {
			listing.NeedsInput++
		}
		for _, next := range childrenOf[current.id] {
			queue = append(queue, queued{id: next, depth: current.depth + 1})
		}
	}
	sort.SliceStable(listing.Members, func(i, j int) bool {
		if listing.Members[i].Depth != listing.Members[j].Depth {
			return listing.Members[i].Depth < listing.Members[j].Depth
		}
		return listing.Members[i].UpdatedAt > listing.Members[j].UpdatedAt
	})
	return listing, true
}

// effectiveParent prefers the user's display grouping when set, so "Make main
// session" and manual regrouping move a lane's team membership too, then falls
// back to the trusted creator lineage.
func effectiveParent(info state.SessionInfo) string {
	if info.DisplayParentSessionID != nil && *info.DisplayParentSessionID != "" {
		return *info.DisplayParentSessionID
	}
	return info.ParentSessionID
}

// handleTeamRoute serves GET /api/lanes/mine. The caller identifies itself with
// the same creator-session header a child uses to record authorship, or with
// ?lane=<id> for a person inspecting a lane's team from a client. An unknown or
// missing caller is a 400, not the whole fleet: this endpoint never widens.
func (s *Server) handleTeamRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	if request.URL.Path != "/api/lanes/mine" || request.Method != http.MethodGet {
		return false
	}
	caller := strings.TrimSpace(request.URL.Query().Get("lane"))
	if caller == "" {
		if headerValue, present, err := creatorHeaderValue(request.Header, creatorSessionHeader); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		} else if present {
			caller = headerValue
		}
	}
	if caller == "" {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{
			"error": "identify the calling lane with the " + creatorSessionHeader + " header or ?lane=<id>",
		}, corsOrigin)
		return true
	}
	listing, ok := teamFor(s.registry.List(true), caller)
	if !ok {
		s.sendJSON(response, http.StatusNotFound, map[string]any{
			"error": "no session matches " + caller + " on this machine",
		}, corsOrigin)
		return true
	}
	s.sendJSON(response, http.StatusOK, listing, corsOrigin)
	return true
}
