package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/claudep"
	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type idleHookContext struct {
	Summary    string
	Outcome    IdleOutcome
	DurationMS int64
}

func (m *Manager) idleDir() string {
	root := m.config.UserStateRoot
	if root == "" {
		root = m.config.StateRoot
	}
	return filepath.Join(root, "idle")
}

func sessionDisplayLabel(info state.SessionInfo) string {
	for _, value := range []string{info.Name, info.ClaudeCustomTitle, info.ClaudeAITitle} {
		if value != "" {
			return value
		}
	}
	if info.Kind == state.KindLane && info.Cmd != "" {
		return filepath.Base(info.Cmd)
	}
	if base := filepath.Base(info.Cwd); base != "." && base != string(filepath.Separator) && base != "" {
		return base
	}
	if info.Cmd != "" {
		return info.Cmd
	}
	if len(info.ID) > 8 {
		return info.ID[:8]
	}
	return info.ID
}

func (m *Manager) removeIdleSentinel(id string) {
	_ = os.Remove(filepath.Join(m.idleDir(), id))
}

func (m *Manager) writeIdleSentinel(info state.SessionInfo) {
	dir := m.idleDir()
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	body := struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		At   string `json:"at"`
	}{info.ID, sessionDisplayLabel(info), time.Now().UTC().Format("2006-01-02T15:04:05.000Z")}
	encoded, err := json.Marshal(body)
	if err != nil {
		return
	}
	encoded = append(encoded, '\n')
	path := filepath.Join(dir, info.ID)
	if os.WriteFile(path, encoded, 0o600) == nil {
		_ = os.Chmod(path, 0o600)
	}
}

func inspectIdle(session *state.Session) (IdleClassification, string) {
	snapshot, _, err := session.Snapshot(context.Background(), 0)
	if err != nil {
		snapshot = ""
	}
	events := session.ClaudeEventLog()
	classification, authoritative := structuredIdleClassification(session.Info().Kind, events)
	if !authoritative {
		classification = ClassifySnapshot(snapshot)
	}
	summary := FinalAssistantSummary(events)
	if summary == "" {
		summary = mirrorTailSummary(snapshot)
	}
	return classification, summary
}

func structuredIdleClassification(kind string, events []json.RawMessage) (IdleClassification, bool) {
	for index := len(events) - 1; index >= 0; index-- {
		var event struct {
			Source  string `json:"source"`
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			Status  string `json:"status"`
			IsError bool   `json:"is_error"`
			Result  string `json:"result"`
			Error   any    `json:"error"`
		}
		if json.Unmarshal(events[index], &event) != nil {
			continue
		}
		switch kind {
		case state.KindCodexAppServer:
			if event.Source != codexapp.HistorySource {
				continue
			}
			// An approval the runner is holding open is the reason the lane
			// stopped; it is answered, not replied to.
			if event.Subtype == "approval_requested" {
				return IdleClassification{Outcome: IdleBlocked, Line: approvalLine(events[index])}, true
			}
			if event.Subtype != "turn_completed" {
				continue
			}
			if strings.EqualFold(event.Status, "completed") {
				if question, asked := AssistantQuestion(events[:index+1]); asked {
					return IdleClassification{Outcome: IdleBlocked, Line: question}, true
				}
				return IdleClassification{Outcome: IdleDone}, true
			}
			return IdleClassification{
				Outcome: IdleError,
				Line:    structuredFailureDetail(event.Status, event.Error, ""),
			}, true
		case state.KindClaudeStructured:
			if event.Source != claudep.HistorySource || event.Type != "result" {
				continue
			}
			if !event.IsError && strings.EqualFold(event.Subtype, "success") {
				if question, asked := AssistantQuestion(events[:index+1]); asked {
					return IdleClassification{Outcome: IdleBlocked, Line: question}, true
				}
				return IdleClassification{Outcome: IdleDone}, true
			}
			return IdleClassification{
				Outcome: IdleError,
				Line:    structuredFailureDetail(event.Subtype, event.Error, event.Result),
			}, true
		}
	}
	return IdleClassification{}, false
}

func structuredFailureDetail(status string, raw any, result string) string {
	if object, ok := raw.(map[string]any); ok {
		if message, ok := object["message"].(string); ok {
			if detail := conciseText(message, 180); detail != "" {
				return detail
			}
		}
	}
	if message, ok := raw.(string); ok {
		if detail := conciseText(message, 180); detail != "" {
			return detail
		}
	}
	if detail := conciseText(result, 180); detail != "" {
		return detail
	}
	if detail := conciseText(status, 180); detail != "" {
		return detail
	}
	return "provider turn failed"
}

func idleReason(outcome IdleOutcome) string {
	switch outcome {
	case IdleBlocked:
		return state.IdleReasonNeedsInput
	case IdleError:
		return state.IdleReasonFailed
	default:
		return state.IdleReasonCompleted
	}
}

// preTurnInspectInterval bounds how often a quiet, never-started terminal is
// re-read. A dialog is drawn once; cursor blinks and resizes redraw it, and
// each redraw is a few bytes that would otherwise trigger a snapshot per tick.
var preTurnInspectInterval = 500 * time.Millisecond

// inspectPreTurn classifies a provider terminal that has produced output and
// gone quiet before its first turn. The working-to-idle edge is the normal
// trigger for classification, but a provider control drawn at launch (Claude's
// folder-trust dialog, a login prompt) never makes the session "working", so
// without this the session reads never-started while a real choice is
// pending, and a message typed into it activates the highlighted option.
//
// Only the blocked outcome is recorded. A quiet never-started session is not
// completed or failed; it is waiting for its first request, and reporting
// anything else would misstate a lifecycle that has not begun. When the
// control is answered and the screen no longer shows it, the session returns
// to never-started so the composer opens again.
func (r *runtimeSession) inspectPreTurn(recentBytes int) {
	info := r.session.Info()
	if info.Exited || info.Working || !supportsTurnLifecycle(info) ||
		info.Kind == state.KindClaudeStructured || info.Kind == state.KindCodexAppServer {
		return
	}
	if recentBytes >= workingBytesThreshold {
		return
	}
	r.mu.Lock()
	pending := r.preTurnOutput
	blocked := r.preTurnBlocked
	last := r.preTurnInspectedAt
	r.mu.Unlock()
	if !pending || time.Since(last) < preTurnInspectInterval {
		return
	}
	if info.IdleReason != state.IdleReasonNeverStarted && !(blocked && info.IdleReason == state.IdleReasonNeedsInput) {
		return
	}
	snapshot, _, err := r.session.Snapshot(context.Background(), 0)
	if err != nil {
		return
	}
	classification := ClassifySnapshot(snapshot)
	now := time.Now()
	r.mu.Lock()
	r.preTurnOutput = false
	r.preTurnInspectedAt = now
	switch {
	case classification.Outcome == IdleBlocked:
		r.preTurnBlocked = true
	case blocked:
		r.preTurnBlocked = false
	}
	r.mu.Unlock()
	switch {
	case classification.Outcome == IdleBlocked:
		r.session.SetIdleResult(state.IdleReasonNeedsInput, classification.Line, "", now.UnixMilli())
	case blocked:
		r.session.SetIdleResult(state.IdleReasonNeverStarted, "", "", now.UnixMilli())
	}
}

func (m *Manager) handleIdle(session *state.Session, duration time.Duration) IdleClassification {
	info := session.Info()
	if info.Exited {
		return IdleClassification{Outcome: IdleDone}
	}
	classification, summary := inspectIdle(session)
	return m.publishIdle(session, duration, classification, summary)
}

func (m *Manager) handleCompletedTurn(session *state.Session, duration time.Duration) IdleClassification {
	summary := FinalAssistantSummary(session.ClaudeEventLog())
	return m.publishIdle(session, duration, IdleClassification{Outcome: IdleDone}, summary)
}

func (m *Manager) publishIdle(session *state.Session, duration time.Duration, classification IdleClassification, summary string) IdleClassification {
	info := session.Info()
	m.observe(context.Background(), "idle", func(writer ledger.ObservationWriter) error {
		return writer.RecordIdle(context.Background(), ledger.Observation{Meta: ledger.Meta{LaneID: info.ID}})
	})
	session.SetIdleResult(idleReason(classification.Outcome), classification.Line, summary, time.Now().UnixMilli())
	info = session.Info()
	hookContext := idleHookContext{Summary: summary, Outcome: classification.Outcome, DurationMS: duration.Milliseconds()}
	m.writeIdleSentinel(info)
	m.runHook(info.OnIdle, info, hookContext, false)
	m.runHook(m.hooks.OnIdle, info, hookContext, true)
	return classification
}

func (m *Manager) runHook(script string, info state.SessionInfo, hook idleHookContext, timeout bool) {
	if script == "" {
		return
	}
	command := newIdleHookCommand(script)
	command.Dir = info.Cwd
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	command.Env = hookEnvironment(info, hook)
	if command.Start() != nil {
		return
	}
	if !timeout {
		go func() { _ = command.Wait() }()
		return
	}
	done := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(done)
	}()
	go func() {
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			_ = command.Process.Kill()
		}
	}()
}

func hookEnvironment(info state.SessionInfo, hook idleHookContext) []string {
	environment := make(map[string]string)
	for _, entry := range os.Environ() {
		if index := strings.IndexByte(entry, '='); index >= 0 {
			environment[entry[:index]] = entry[index+1:]
		}
	}
	environment["SESSIONS_SESSION_ID"] = info.ID
	environment["SESSIONS_SESSION_NAME"] = sessionDisplayLabel(info)
	environment["SESSIONS_SESSION_TOOL"] = string(info.Tool)
	environment["SESSIONS_SESSION_CWD"] = info.Cwd
	environment["SESSIONS_FINAL_MESSAGE"] = hook.Summary
	environment["SESSIONS_OUTCOME"] = string(hook.Outcome)
	environment["SESSIONS_DURATION_MS"] = strconv.FormatInt(hook.DurationMS, 10)
	result := make([]string, 0, len(environment))
	for key, value := range environment {
		result = append(result, key+"="+value)
	}
	return result
}

func approvalLine(raw json.RawMessage) string {
	var event struct {
		Approval struct {
			Summary string `json:"summary"`
		} `json:"approval"`
	}
	if json.Unmarshal(raw, &event) == nil && strings.TrimSpace(event.Approval.Summary) != "" {
		return "Allow? " + strings.TrimSpace(event.Approval.Summary)
	}
	return "Allow? The lane asked for permission"
}
