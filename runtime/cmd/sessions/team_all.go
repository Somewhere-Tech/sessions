package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// teamRollup is one manager as a person sees it from the top: how many lanes
// it delegated, how many are working, and which ones wait on someone.
type teamRollup struct {
	ID       string        `json:"id"`
	Name     string        `json:"name,omitempty"`
	Tool     string        `json:"tool"`
	Cwd      string        `json:"cwd,omitempty"`
	State    string        `json:"state"`
	Lanes    int           `json:"lanes"`
	Working  int           `json:"working"`
	NeedsYou int           `json:"needs_you"`
	Waiting  []teamWaiting `json:"waiting,omitempty"`
	Summary  string        `json:"summary,omitempty"`
}

type teamWaiting struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Line string `json:"line,omitempty"`
}

func sessionEffectiveParent(s session) string {
	if s.DisplayParentSessionID != nil && *s.DisplayParentSessionID != "" {
		return *s.DisplayParentSessionID
	}
	return s.ParentSessionID
}

func sessionTeamState(s session) string {
	switch {
	case s.Exited:
		return "ended"
	case s.IdleReason == "needs-input":
		return "needs-you"
	case s.Working:
		return "working"
	case s.IdleReason == "failed":
		return "failed"
	case s.IdleReason == "never-started":
		return "not-started"
	default:
		return "idle"
	}
}

// teamRollups folds every live session under its root: the sessions a person
// talks to become rows, and everything delegated beneath each becomes its
// counts. Roots without lanes are left out; they are ordinary sessions.
func teamRollups(sessions []session) []teamRollup {
	byID := make(map[string]session, len(sessions))
	for _, s := range sessions {
		byID[s.ID] = s
	}
	rootOf := func(s session) string {
		current := s
		for depth := 0; depth < 16; depth++ {
			parent := sessionEffectiveParent(current)
			next, ok := byID[parent]
			if parent == "" || !ok {
				return current.ID
			}
			current = next
		}
		return current.ID
	}
	rollups := make(map[string]*teamRollup)
	for _, s := range sessions {
		if s.Exited {
			continue
		}
		root := rootOf(s)
		if root == s.ID {
			continue
		}
		rollup, ok := rollups[root]
		if !ok {
			manager := byID[root]
			rollup = &teamRollup{
				ID: manager.ID, Name: manager.Name, Tool: manager.Tool, Cwd: manager.Cwd,
				State: sessionTeamState(manager), Summary: oneLine(manager.LastSummary),
			}
			rollups[root] = rollup
		}
		rollup.Lanes++
		switch sessionTeamState(s) {
		case "working":
			rollup.Working++
		case "needs-you":
			rollup.NeedsYou++
			line := s.IdleDetail
			if line == "" {
				line = "waiting for a decision"
			}
			rollup.Waiting = append(rollup.Waiting, teamWaiting{ID: s.ID, Name: s.Name, Line: oneLine(line)})
		}
	}
	result := make([]teamRollup, 0, len(rollups))
	for _, rollup := range rollups {
		result = append(result, *rollup)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].NeedsYou != result[j].NeedsYou {
			return result[i].NeedsYou > result[j].NeedsYou
		}
		if result[i].Working != result[j].Working {
			return result[i].Working > result[j].Working
		}
		return result[i].Name < result[j].Name
	})
	return result
}

func (a *app) cmdTeamAll() error {
	sessions, err := a.listSessions(false)
	if err != nil {
		return err
	}
	rollups := teamRollups(sessions)
	if a.wantJSON {
		return writeJSON(a.stdout, map[string]any{"managers": rollups}, true)
	}
	if len(rollups) == 0 {
		_, err := io.WriteString(a.stdout, "no session has delegated lanes right now\n")
		return err
	}
	rows := [][]string{{"ID", "MANAGER", "TOOL", "CWD", "LANES", "WORKING", "NEEDS YOU"}}
	for _, rollup := range rollups {
		name := rollup.Name
		if strings.TrimSpace(name) == "" {
			name = "-"
		}
		rows = append(rows, []string{
			shortID(rollup.ID), name, rollup.Tool, a.homeRelative(rollup.Cwd),
			fmt.Sprint(rollup.Lanes), fmt.Sprint(rollup.Working), fmt.Sprint(rollup.NeedsYou),
		})
	}
	if err := writePaddedRows(a.stdout, rows); err != nil {
		return err
	}
	waiting := 0
	for _, rollup := range rollups {
		for _, lane := range rollup.Waiting {
			if waiting == 0 {
				if _, err := io.WriteString(a.stdout, "\nwaiting on you:\n"); err != nil {
					return err
				}
			}
			waiting++
			name := lane.Name
			if strings.TrimSpace(name) == "" {
				name = shortID(lane.ID)
			}
			if _, err := fmt.Fprintf(a.stdout, "  %s  %s — %s  (under %s)\n", shortID(lane.ID), name, lane.Line, rollup.Name); err != nil {
				return err
			}
		}
	}
	if waiting > 0 {
		_, err := io.WriteString(a.stdout, "answer with `sessions ask <id>`, allow with `sessions approve <id>`, or `sessions team <manager-id>` for one team\n")
		return err
	}
	return nil
}
