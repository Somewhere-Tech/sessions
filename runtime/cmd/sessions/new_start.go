package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// Starting delegated work is two operations -- create a session, then deliver
// its first request -- and either can fail after the other succeeded. Every
// `sessions new` therefore carries a create operation id, and a first request
// its own delivery operation id, both recorded by the daemon before anything
// launches. Re-running the same command with the same --operation-id returns
// the session the first attempt created and sends the first request at most
// once, so a lost response or a failed delivery never has to be answered by
// starting the work again.

const initialRequestTimeout = 30 * time.Second

func pluckStartOperationID(args *[]string) (string, error) {
	value, present := pluck(args, "--operation-id")
	if !present {
		return "", nil
	}
	if !sendOperationIDPattern.MatchString(value) {
		return "", fail(1, "--operation-id must be a lowercase UUID v4 (reuse the one an earlier `sessions new` reported)")
	}
	return value, nil
}

// newStartResult is the additive part of `sessions new --json`: the created
// session record keeps every field it always had, with these beside it.
type newStartResult struct {
	OK           bool                `json:"ok"`
	Code         int                 `json:"code"`
	Error        string              `json:"error,omitempty"`
	FirstRequest *sendJSONResult     `json:"first_request,omitempty"`
	Start        *state.StartReceipt `json:"start,omitempty"`
	StartError   string              `json:"start_error,omitempty"`
	Rerun        string              `json:"rerun,omitempty"`
}

func (a *app) createAndStart(body createSessionRequest, initialInput string) error {
	var err error
	if body.OperationID == "" {
		if body.OperationID, err = randomUUID(); err != nil {
			return fail(1, "create session operation id: %s", err)
		}
	}
	if strings.TrimSpace(initialInput) != "" {
		if body.PromptOperationID, err = randomUUID(); err != nil {
			return fail(1, "create first request operation id: %s", err)
		}
	}
	a.announceStartupOnce()
	info, failure := a.postCreate(body)
	if failure != nil {
		return a.writeCreateFailure(*failure)
	}
	id := strings.TrimSpace(fmt.Sprint(info["id"]))
	if id == "" || info["id"] == nil {
		return a.writeCreateFailure(createFailure{Outcome: createOutcomeUnknown, OperationID: body.OperationID,
			Error: "sessionsd answered the create without a session id", Next: unknownCreateNext(body.OperationID)})
	}
	result := newStartResult{OK: true}
	if strings.TrimSpace(initialInput) != "" {
		promptOperationID := recordedPromptOperationID(info, body.PromptOperationID)
		sent, sendErr := a.sendExplicitOperation(id, initialInput, "", promptOperationID, initialRequestTimeout, false)
		result.FirstRequest = firstRequestResult(sent, promptOperationID, sendErr)
		if sendErr != nil || sent.ExitCode != 0 {
			result.OK, result.Code, result.Rerun = false, exitTransport, rerunAdvice(info, body.OperationID, id, promptOperationID)
			result.Error = fmt.Sprintf("session %s was created, but its first request was not confirmed: %s", id, firstRequestProblem(sent, sendErr))
		}
	}
	current, start, startError := a.readCurrentSession(id)
	result.Start, result.StartError = start, startError
	if current != nil {
		info = currentCreateRecord(info, current, result.Start)
	}
	return a.writeNewStartResult(id, info, result)
}

// currentCreateRecord answers with the record read after the first request
// rather than the create answer, so working, idleReason and start.phase in one
// document describe the same moment. Only the create answer knows it was a
// replay, so that mark is carried onto the fresh receipt.
func currentCreateRecord(created, current map[string]any, start *state.StartReceipt) map[string]any {
	createdStart, _ := created["start"].(map[string]any)
	if replayed, _ := createdStart["replayed"].(bool); replayed && start != nil {
		start.Replayed = true
	}
	return current
}

// rerunAdvice promises an idempotent re-run only when this daemon echoed the
// operation id back, which is the proof it recorded it. An older daemon
// ignores the field, and re-running against it would start a second session.
func rerunAdvice(info map[string]any, operationID, sessionID, promptOperationID string) string {
	start, _ := info["start"].(map[string]any)
	if recorded, _ := start["operation_id"].(string); recorded == operationID {
		return fmt.Sprintf("re-run the same `sessions new` command with --operation-id %s: it returns session %s and sends the first request at most once", operationID, sessionID)
	}
	return fmt.Sprintf("this sessionsd did not record the operation id, so re-running `sessions new` would start a second session; send the first request to session %s with `sessions send %s --operation-id %s -- <first request>` instead, and update sessionsd for idempotent starts",
		sessionID, sessionID, promptOperationID)
}

const (
	createOutcomeRefused        = "refused"
	createOutcomeAlreadyCreated = "already-created"
	createOutcomeNotStarted     = "created-not-started"
	createOutcomeUnknown        = "unknown"
)

// createFailure is the structured answer for a create that did not return a
// usable session. Outcome says what is known: refused (no session id returned),
// already-created or created-not-started (session_id names the session), or
// unknown (the answer was lost; a session may exist).
type createFailure struct {
	OK          bool   `json:"ok"`
	Code        int    `json:"code"`
	Error       string `json:"error"`
	Outcome     string `json:"outcome"`
	OperationID string `json:"operation_id"`
	SessionID   string `json:"session_id,omitempty"`
	Next        string `json:"next"`
}

// postCreate distinguishes a refused create, which created nothing, from one
// that recorded a session before failing, and from one whose outcome was lost.
func (a *app) postCreate(body createSessionRequest) (map[string]any, *createFailure) {
	response, err := a.api.requestWithHeaders(context.Background(), http.MethodPost, "/api/sessions", body, 0, nil)
	if err != nil {
		return nil, &createFailure{Outcome: createOutcomeUnknown, OperationID: body.OperationID,
			Error: fmt.Sprintf("/api/sessions → %s", err), Next: unknownCreateNext(body.OperationID)}
	}
	var info map[string]any
	decodeErr := json.Unmarshal(response.body, &info)
	if response.status < 400 && decodeErr == nil {
		return info, nil
	}
	failure := &createFailure{Outcome: createOutcomeUnknown, OperationID: body.OperationID, Next: unknownCreateNext(body.OperationID)}
	failure.Error, _ = info["error"].(string)
	failure.SessionID, _ = info["session_id"].(string)
	if failure.Error == "" {
		failure.Error = fmt.Sprintf("/api/sessions → %d %s", response.status, prefixBytes(response.body, 200))
	}
	switch {
	case failure.SessionID != "" && response.status == http.StatusConflict:
		failure.Outcome, failure.Next = createOutcomeAlreadyCreated, fmt.Sprintf("inspect session %s with `sessions status %s`; do not start the same work again", failure.SessionID, failure.SessionID)
	case failure.SessionID != "":
		failure.Outcome, failure.Next = createOutcomeNotStarted, fmt.Sprintf("session %s was recorded before its launch failed; inspect it with `sessions status %s` before creating another", failure.SessionID, failure.SessionID)
	case response.status >= 400 && response.status < 500:
		failure.Outcome, failure.Next = createOutcomeRefused, "the daemon refused this request without a session id; inspect existing sessions before retrying if this followed an earlier start attempt or used an older daemon"
	}
	return nil, failure
}

// unknownCreateNext is the advice when Sessions cannot tell whether a session
// exists. Whether a re-run is safe depends on the daemon recording operation
// ids, which an unreachable daemon cannot confirm, so inspection comes first.
func unknownCreateNext(operationID string) string {
	return fmt.Sprintf("a session may have been created; look for it with `sessions --json ls` (start.operation_id %s) before retrying. A sessionsd that records operation ids returns that same session when you re-run with --operation-id %s; an older one would start a second session, so update it first", operationID, operationID)
}

func (a *app) writeCreateFailure(failure createFailure) error {
	failure.OK, failure.Code = false, exitTransport
	if a.wantJSON {
		if err := writeJSON(a.stdout, failure, true); err != nil {
			return err
		}
		return status(failure.Code)
	}
	if failure.SessionID != "" {
		if _, err := fmt.Fprintln(a.stdout, failure.SessionID); err != nil {
			return err
		}
	}
	if failure.Outcome == createOutcomeRefused {
		// A refusal created nothing and its message already says why; the
		// human line stays exactly the daemon's sentence.
		return fail(failure.Code, "%s", failure.Error)
	}
	return fail(failure.Code, "%s\n  %s", failure.Error, failure.Next)
}

// recordedPromptOperationID prefers the first request id the daemon recorded
// for a replayed create: sending under that id is what makes the retry unable
// to deliver the request twice.
func recordedPromptOperationID(info map[string]any, fallback string) string {
	start, _ := info["start"].(map[string]any)
	if replayed, _ := start["replayed"].(bool); replayed {
		if recorded, _ := start["prompt_operation_id"].(string); recorded != "" {
			return recorded
		}
	}
	return fallback
}

func firstRequestResult(sent sendResult, operationID string, sendErr error) *sendJSONResult {
	result := &sendJSONResult{OperationID: firstNonBlank(sent.OperationID, operationID), Confidence: sent.Confidence, Reason: sent.Reason, Tool: sent.Tool}
	if sendErr != nil {
		result.Confidence, result.Reason = "unknown", sendErr.Error()
		return result
	}
	result.Submitted = sent.Confirmed
	if sent.Confirmed != nil && !*sent.Confirmed {
		result.Retry = boolPointer(sent.Retry)
	}
	return result
}

func firstRequestProblem(sent sendResult, sendErr error) string {
	if sendErr != nil {
		return sendErr.Error()
	}
	return firstNonBlank(sent.Reason, sent.Confidence, "delivery was not confirmed")
}

// readCurrentSession asks the daemon what it knows now: the session's whole
// record and its start receipt, from one read. A failed read is reported,
// never replaced by a guess about how far the session got.
func (a *app) readCurrentSession(id string) (map[string]any, *state.StartReceipt, string) {
	var response struct {
		Sessions []json.RawMessage `json:"sessions"`
	}
	if err := a.getJSON("/api/sessions", &response); err != nil {
		return nil, nil, "could not read the start receipt: " + err.Error()
	}
	for _, raw := range response.Sessions {
		var current map[string]any
		var decoded struct {
			ID    string              `json:"id"`
			Start *state.StartReceipt `json:"start"`
		}
		if json.Unmarshal(raw, &decoded) != nil || decoded.ID != id || json.Unmarshal(raw, &current) != nil {
			continue
		}
		if decoded.Start == nil {
			return current, nil, "this daemon does not report start receipts; update Sessions to see delivery and working evidence"
		}
		return current, decoded.Start, ""
	}
	return nil, nil, fmt.Sprintf("session %s is no longer listed; inspect it with `sessions status %s`", id, id)
}

func (a *app) writeNewStartResult(id string, info map[string]any, result newStartResult) error {
	if a.wantJSON {
		encoded, err := json.Marshal(result)
		if err != nil {
			return err
		}
		var additions map[string]any
		if err := json.Unmarshal(encoded, &additions); err != nil {
			return err
		}
		for key, value := range additions {
			info[key] = value
		}
		if err := writeJSON(a.stdout, info, true); err != nil {
			return err
		}
		return statusOrNil(result.Code)
	}
	if _, err := fmt.Fprintln(a.stdout, id); err != nil {
		return err
	}
	if result.OK {
		return nil
	}
	return fail(result.Code, "%s%s\n  %s", result.Error, startRecoveryText(result.Start, result.StartError), result.Rerun)
}

func startRecoveryText(start *state.StartReceipt, startError string) string {
	if startError != "" {
		return "\n  " + startError
	}
	if start == nil {
		return ""
	}
	text := fmt.Sprintf("\n  start: %s — %s", start.Phase, start.Evidence)
	if start.Recovery != nil {
		text += "\n  next: " + start.Recovery.Detail
		if start.Recovery.Command != "" {
			text += "\n        " + start.Recovery.Command
		}
	}
	return text
}

func statusOrNil(code int) error {
	if code == 0 {
		return nil
	}
	return status(code)
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
