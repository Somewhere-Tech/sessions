package main

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// teamMember mirrors the compact fact set the daemon returns for one lane a
// caller is responsible for. It carries no transcript, args, or environment by
// design; a manager watches its workers here without pulling their
// conversations into its own context.
type teamMember struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Tool     string `json:"tool"`
	Cwd      string `json:"cwd,omitempty"`
	Relation string `json:"relation"`
	Depth    int    `json:"depth"`
	State    string `json:"state"`
	NeedsYou bool   `json:"needs_you"`
	Working  bool   `json:"working"`
	Exited   bool   `json:"exited"`
	Summary  string `json:"summary,omitempty"`
	Waiting  string `json:"waiting,omitempty"`
}

type teamListing struct {
	Self       *teamMember  `json:"self,omitempty"`
	Parent     *teamMember  `json:"parent,omitempty"`
	Members    []teamMember `json:"members"`
	NeedsInput int          `json:"needs_input"`
}

// cmdTeam shows the lanes a caller is responsible for: its parent and its
// delegated descendants, with each one's state and last line of work. The
// caller is the SESSIONS_SESSION_ID of the invoking lane, or an explicit id so
// a person can inspect any lane's team.
func (a *app) cmdTeam(args []string) error {
	lane := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		lane = args[0]
		args = args[1:]
	}
	if len(args) > 0 {
		return fail(1, "usage: sessions team [lane-id]")
	}
	if lane == "" {
		lane = strings.TrimSpace(os.Getenv("SESSIONS_SESSION_ID"))
	}
	if lane == "" {
		return fail(1, "no calling lane: run this inside a Sessions lane, or pass `sessions team <lane-id>`")
	}

	path := "/api/lanes/mine?lane=" + escapeID(lane)
	var listing teamListing
	if err := a.getJSON(path, &listing); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, listing, true)
	}
	return a.writeTeam(listing)
}

func (a *app) writeTeam(listing teamListing) error {
	if listing.Parent != nil {
		if _, err := io.WriteString(a.stdout, "manager: "+a.teamLine(*listing.Parent)+"\n\n"); err != nil {
			return err
		}
	}
	if len(listing.Members) == 0 {
		_, err := io.WriteString(a.stdout, "no lanes delegated from this session\n")
		return err
	}
	rows := [][]string{{"ID", "NAME", "TOOL", "CWD", "STATE", "LAST"}}
	for _, member := range listing.Members {
		name := member.Name
		if strings.TrimSpace(name) == "" {
			name = "-"
		}
		last := member.Summary
		if member.NeedsYou && member.Waiting != "" {
			last = member.Waiting
		}
		if strings.TrimSpace(last) == "" {
			last = "-"
		}
		indent := strings.Repeat("  ", maxInt(0, member.Depth-1))
		rows = append(rows, []string{
			shortID(member.ID), indent + name, member.Tool,
			a.homeRelative(member.Cwd), member.State, oneLine(last),
		})
	}
	if err := writePaddedRows(a.stdout, rows); err != nil {
		return err
	}
	if listing.NeedsInput > 0 {
		if _, err := io.WriteString(a.stdout, "\n"+pluralLanes(listing.NeedsInput)+" waiting on you; answer with `sessions ask <id>`, allow with `sessions approve <id>`, or open the lane\n"); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) teamLine(member teamMember) string {
	name := member.Name
	if strings.TrimSpace(name) == "" {
		name = shortID(member.ID)
	}
	return name + " (" + member.State + ")"
}

// homeRelative shortens a path under the user's home to ~ on a path-component
// boundary, so /Users/x/work becomes ~/work while /Users/x-tools stays whole.
func (a *app) homeRelative(path string) string {
	if a.home == "" || path == "" {
		return path
	}
	home := a.home
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if path == home {
		return "~"
	}
	prefix := home
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	if strings.HasPrefix(path, prefix) {
		return "~/" + path[len(prefix):]
	}
	return path
}

func oneLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func pluralLanes(count int) string {
	if count == 1 {
		return "1 lane"
	}
	return strconv.Itoa(count) + " lanes"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
