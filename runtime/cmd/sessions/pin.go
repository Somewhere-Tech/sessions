package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// pinResult is the --json answer for pin and unpin. ok and code are here for
// the same reason every other envelope carries them: `sessions help` promises
// an agent that every --json document says what the exit status said, and a
// document that leaves the promise unkept is read as success by the ordinary
// way of reading a missing number.
type pinResult struct {
	OK     bool   `json:"ok"`
	Code   int    `json:"code"`
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Pinned bool   `json:"pinned"`
}

func (a *app) cmdPin(args []string) error   { return a.setPinned(args, true, "pin") }
func (a *app) cmdUnpin(args []string) error { return a.setPinned(args, false, "unpin") }

// setPinned is the whole of both verbs. They are separate commands rather than
// one command with a flag because unpinning is the thing a user reaches for
// when a pinned session is in the way, and `sessions unpin bolo` is what they
// will type; aside uses --clear because setting aside is one direction of a
// working-set edit, while a pin is a mark that is either on or off.
func (a *app) setPinned(args []string, pinned bool, verb string) error {
	usage := "usage: sessions " + verb + " <session>"
	targets := make([]string, 0, len(args))
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			return fail(1, "%s", usage)
		}
		targets = append(targets, arg)
	}
	if len(targets) != 1 {
		return fail(1, "%s", usage)
	}
	id, err := a.resolveSessionID(targets[0])
	if err != nil {
		return err
	}

	path := "/api/sessions/" + escapeID(id) + "/pin"
	response, err := a.api.request(context.Background(), http.MethodPut, path, map[string]any{"pinned": pinned}, 0)
	if err != nil {
		return fail(2, "%s → %s", path, err)
	}
	if response.status >= 400 {
		var payload struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(response.body, &payload)
		if payload.Error != "" {
			return fail(2, "%s", payload.Error)
		}
		return fail(2, "%s → %d %s", path, response.status, prefixBytes(response.body, 200))
	}
	// The daemon answers with the state it actually stored. Reporting the
	// requested value instead would hide a daemon that accepted the call and
	// did something else, which is the one failure a caller cannot see.
	var stored struct {
		Pinned bool `json:"pinned"`
	}
	if err := json.Unmarshal(response.body, &stored); err != nil {
		return fail(2, "%s → undecodable answer %s", path, prefixBytes(response.body, 200))
	}

	name := ""
	if current, resolveErr := a.resolveStatusSession(id); resolveErr == nil {
		name = strings.TrimSpace(current.Name)
	}
	if a.wantJSON {
		return writeJSON(a.stdout, pinResult{
			OK: true, Code: exitSatisfied, ID: id, Name: name, Pinned: stored.Pinned,
		}, true)
	}
	state := "unpinned"
	if stored.Pinned {
		state = "pinned"
	}
	_, err = fmt.Fprintf(a.stdout, "%-8s %s  %s\n", state, prefixString(id, 8), name)
	return err
}
