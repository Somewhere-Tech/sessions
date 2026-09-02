package codexapp

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// ApprovalKind names what Codex is asking permission for.
type ApprovalKind string

const (
	ApprovalCommand     ApprovalKind = "command"
	ApprovalFileChange  ApprovalKind = "file-change"
	ApprovalPermissions ApprovalKind = "permissions"
)

// ApprovalDecision is the answer Sessions gives to one approval request. The
// strings are shared with the runner protocol and the HTTP API so a decision
// reads the same from the app, the CLI, and a lane's transcript.
type ApprovalDecision string

const (
	ApprovalAllow           ApprovalDecision = "allow"
	ApprovalAllowForSession ApprovalDecision = "allow-session"
	ApprovalDeny            ApprovalDecision = "deny"
)

// ApprovalRequest is one server-side approval request in Sessions' words,
// independent of which app-server protocol generation produced it.
type ApprovalRequest struct {
	Kind           ApprovalKind
	ConversationID string
	TurnID         string
	ItemID         string
	Command        string
	CWD            string
	Reason         string
	// Permissions is the profile Codex asked for, echoed back when granted.
	Permissions json.RawMessage
}

// Summary is the one line a person or a manager sees before deciding.
func (r ApprovalRequest) Summary() string {
	reason := strings.TrimSpace(r.Reason)
	switch r.Kind {
	case ApprovalCommand:
		command := strings.TrimSpace(r.Command)
		if command == "" {
			command = "a command"
		} else {
			command = "`" + truncateSummary(command, 160) + "`"
		}
		if reason != "" {
			return "Run " + command + " — " + reason
		}
		return "Run " + command
	case ApprovalPermissions:
		if reason != "" {
			return "Grant more access — " + reason
		}
		return "Grant more access"
	default:
		if reason != "" {
			return "Change files — " + reason
		}
		return "Change files"
	}
}

func truncateSummary(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= limit {
		return value
	}
	ellipsis := "…"
	if limit < len(ellipsis) {
		return ""
	}
	maxPrefix := limit - len(ellipsis)
	cut := 0
	for index := range value {
		if index > maxPrefix {
			break
		}
		cut = index
	}
	return value[:cut] + ellipsis
}

// ApprovalHandler decides one approval request. It runs off the read loop and
// may block for as long as the person or the manager takes to answer.
type ApprovalHandler func(context.Context, ApprovalRequest) ApprovalDecision

// HandleApprovals routes every approval request through handler instead of
// accepting it. Without a handler the client accepts for the session, which is
// what a fully autonomous lane wants.
func (c *Client) HandleApprovals(handler ApprovalHandler) {
	c.mu.Lock()
	c.approvals = handler
	c.mu.Unlock()
}

func isApprovalMethod(method string) bool {
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval",
		"applyPatchApproval", "execCommandApproval":
		return true
	}
	return false
}

// parseApprovalRequest reads the request shape of either protocol generation.
func parseApprovalRequest(method string, params json.RawMessage) ApprovalRequest {
	var raw struct {
		ThreadID       string          `json:"threadId"`
		ConversationID string          `json:"conversationId"`
		TurnID         string          `json:"turnId"`
		ItemID         string          `json:"itemId"`
		CallID         string          `json:"callId"`
		Command        json.RawMessage `json:"command"`
		CWD            string          `json:"cwd"`
		Reason         string          `json:"reason"`
		Permissions    json.RawMessage `json:"permissions"`
	}
	_ = json.Unmarshal(params, &raw)
	request := ApprovalRequest{
		ConversationID: raw.ThreadID, TurnID: raw.TurnID, ItemID: raw.ItemID,
		CWD: raw.CWD, Reason: raw.Reason, Command: commandText(raw.Command),
	}
	if request.ConversationID == "" {
		request.ConversationID = raw.ConversationID
	}
	if request.ItemID == "" {
		request.ItemID = raw.CallID
	}
	switch method {
	case "item/commandExecution/requestApproval", "execCommandApproval":
		request.Kind = ApprovalCommand
	case "item/permissions/requestApproval":
		request.Kind = ApprovalPermissions
		request.Permissions = raw.Permissions
		if len(request.Permissions) == 0 {
			request.Permissions = json.RawMessage(`{}`)
		}
	default:
		request.Kind = ApprovalFileChange
	}
	return request
}

// commandText accepts the string form of the current protocol and the argv
// form of the legacy one.
func commandText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var argv []string
	if json.Unmarshal(raw, &argv) == nil {
		return strings.Join(argv, " ")
	}
	return ""
}

// approvalReply encodes a decision in the vocabulary of the request's protocol
// generation.
func approvalReply(method string, request ApprovalRequest, decision ApprovalDecision) any {
	switch method {
	case "item/permissions/requestApproval":
		reply := struct {
			Permissions json.RawMessage `json:"permissions"`
			Scope       string          `json:"scope"`
		}{Permissions: request.Permissions, Scope: "turn"}
		switch decision {
		case ApprovalAllowForSession:
			reply.Scope = "session"
		case ApprovalDeny:
			reply.Permissions = json.RawMessage(`{}`)
		}
		return reply
	case "applyPatchApproval", "execCommandApproval":
		value := "approved"
		switch decision {
		case ApprovalAllowForSession:
			value = "approved_for_session"
		case ApprovalDeny:
			value = "denied"
		}
		return struct {
			Decision string `json:"decision"`
		}{Decision: value}
	default:
		value := "accept"
		switch decision {
		case ApprovalAllowForSession:
			value = "acceptForSession"
		case ApprovalDeny:
			value = "decline"
		}
		return struct {
			Decision string `json:"decision"`
		}{Decision: value}
	}
}

// ApprovalRequestedEvent records that a lane is waiting on a decision. The
// daemon reads it to mark the session needs-you and to know what is being
// asked; the transcript shows it in place.
func ApprovalRequestedEvent(id string, request ApprovalRequest, at time.Time) (json.RawMessage, error) {
	return marshalHistory(map[string]any{
		"type": "system", "subtype": "approval_requested", "source": HistorySource,
		"timestamp": historyTimestamp(at), "conversationId": request.ConversationID, "turnId": request.TurnID,
		"approval": map[string]any{
			"id": id, "kind": string(request.Kind), "summary": request.Summary(),
			"command": request.Command, "cwd": request.CWD, "reason": request.Reason,
		},
	})
}

// ApprovalResolvedEvent records the answer and who gave it: empty `by` is the
// person, otherwise the session id of the lane that decided.
func ApprovalResolvedEvent(conversationID, id string, decision ApprovalDecision, by string, at time.Time) (json.RawMessage, error) {
	return marshalHistory(map[string]any{
		"type": "system", "subtype": "approval_resolved", "source": HistorySource,
		"timestamp": historyTimestamp(at), "conversationId": conversationID,
		"approval": map[string]any{"id": id, "decision": string(decision), "by": by},
	})
}
