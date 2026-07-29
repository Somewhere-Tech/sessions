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
			if event.Source != codexapp.HistorySource || event.Subtype != "turn_completed" {
				continue
			}
			if strings.EqualFold(event.Status, "completed") {
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

func (m *Manager) handleIdle(session *state.Session, duration time.Duration) IdleClassification {
	info := session.Info()
	if info.Exited {
		return IdleClassification{Outcome: IdleDone}
	}
	m.observe(context.Background(), "idle", func(writer ledger.ObservationWriter) error {
		return writer.RecordIdle(context.Background(), ledger.Observation{Meta: ledger.Meta{LaneID: info.ID}})
	})
	classification, summary := inspectIdle(session)
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
