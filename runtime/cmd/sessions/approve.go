package main

import (
	"io"
	"net/http"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type approveResult struct {
	OK       bool                  `json:"ok"`
	ID       string                `json:"id"`
	Decision string                `json:"decision"`
	Approval *state.ApprovalPrompt `json:"approval,omitempty"`
}

// cmdApprove answers the permission a Rich Codex lane is waiting on. Run from
// inside a manager lane it is attributed to that lane, so the worker's
// transcript says who decided.
func (a *app) cmdApprove(args []string) error {
	deny := removeFirst(&args, "--deny")
	forSession := removeFirst(&args, "--for-session")
	if deny && forSession {
		return fail(2, "--deny and --for-session cannot be combined")
	}
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return fail(2, "usage: sessions approve <session-id> [--deny | --for-session]")
	}
	id, err := a.resolveSessionID(args[0])
	if err != nil {
		return err
	}
	decision := "allow"
	switch {
	case deny:
		decision = "deny"
	case forSession:
		decision = "allow-session"
	}
	headers := http.Header{}
	if creator := strings.TrimSpace(a.api.creatorSession); creator != "" {
		headers.Set("X-Sessions-Creator-Session", creator)
	}
	var result approveResult
	if err := a.postJSONWithHeaders("/api/sessions/"+escapeID(id)+"/approve", map[string]string{"decision": decision}, &result, 1, headers); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, result, true)
	}
	verb := map[string]string{"allow": "Allowed", "allow-session": "Allowed for the rest of the session", "deny": "Declined"}[decision]
	what := ""
	if result.Approval != nil {
		what = ": " + oneLine(result.Approval.Summary)
	}
	_, err = io.WriteString(a.stdout, verb+what+"\n")
	return err
}
