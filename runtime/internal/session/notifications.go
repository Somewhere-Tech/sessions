package session

import (
	"encoding/json"
	"log"
	"path/filepath"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/claudep"
	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type queuedSessionNotification struct {
	kind    string
	payload PushPayload
}

type sessionNotificationState struct {
	lastSent time.Time
	pending  *queuedSessionNotification
	timer    *time.Timer
}

func structuredTurnCompleted(kind string, raw json.RawMessage) bool {
	var event struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Source  string `json:"source"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return false
	}
	switch kind {
	case state.KindCodexAppServer:
		return event.Source == codexapp.HistorySource && event.Subtype == "turn_completed"
	case state.KindClaudeStructured:
		return event.Source == claudep.HistorySource && event.Type == "result"
	default:
		return false
	}
}

func (r *runtimeSession) markStructuredDone() {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.structuredDone = true
	r.cancelWaitingLocked()
	r.mu.Unlock()

	r.notifyDone()
}

func (r *runtimeSession) notifyDone() {
	info := r.session.Info()
	body := info.LastSummary
	if body == "" {
		body = FinalAssistantSummary(r.session.ClaudeEventLog())
	}
	if body == "" {
		body = "finished"
	}
	r.manager.queueSessionNotification(info.ID, state.NotifyDone, PushPayload{
		Title: "🟢 " + sessionDisplayLabel(info) + " — done",
		Body:  body,
		Data:  map[string]any{"sessionId": info.ID, "notification": state.NotifyDone},
	})
}

func (r *runtimeSession) cancelWaiting() {
	r.mu.Lock()
	r.cancelWaitingLocked()
	r.mu.Unlock()
}

func (r *runtimeSession) cancelWaitingLocked() {
	r.waitingGeneration++
	if r.waitingTimer != nil {
		r.waitingTimer.Stop()
		r.waitingTimer = nil
	}
}

func (r *runtimeSession) scheduleWaiting() {
	info := r.session.Info()
	if info.Exited || info.Working {
		return
	}
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.cancelWaitingLocked()
	generation := r.waitingGeneration
	r.waitingTimer = time.AfterFunc(r.manager.options.NotifyWaitingDelay, func() {
		r.fireWaiting(generation)
	})
	r.mu.Unlock()
}

func (r *runtimeSession) fireWaiting(generation uint64) {
	r.mu.Lock()
	if r.stopped || generation != r.waitingGeneration {
		r.mu.Unlock()
		return
	}
	r.waitingTimer = nil
	r.mu.Unlock()

	info := r.session.Info()
	if info.Exited || info.Working {
		return
	}
	body := info.LastSummary
	if body == "" {
		body = FinalAssistantSummary(r.session.ClaudeEventLog())
	}
	if body == "" {
		if snapshot, _, err := r.session.Snapshot(r.manager.ctx, 0); err == nil {
			body = mirrorTailSummary(snapshot)
		}
	}
	if body == "" {
		body = "waiting for input"
	}
	title := "🟡 " + sessionDisplayLabel(info) + " — waiting"
	if info.IdleReason == state.IdleReasonNeedsInput {
		title = "🟡 " + sessionDisplayLabel(info) + " — needs you"
		if info.IdleDetail != "" {
			body = info.IdleDetail
		}
	} else if info.IdleReason == state.IdleReasonFailed {
		title = "🔴 " + sessionDisplayLabel(info) + " — failed"
	}
	r.manager.queueSessionNotification(info.ID, state.NotifyWaiting, PushPayload{
		Title: title,
		Body:  body,
		Data: map[string]any{
			"sessionId": info.ID, "notification": state.NotifyWaiting,
			"idleReason": info.IdleReason, "summary": info.LastSummary,
		},
	})
}

func (m *Manager) notifyLost(info state.SessionInfo) {
	m.queueSessionNotification(info.ID, state.NotifyLost, PushPayload{
		Title: "🔴 " + sessionDisplayLabel(info) + " — lost (sessions recover)",
		Body:  "The runner connection was lost. Run `sessions recover` to inspect recovery options.",
		Data:  map[string]any{"sessionId": info.ID, "notification": state.NotifyLost},
	})
}

func (m *Manager) settingsPath() string {
	if m.config.SettingsPath != "" {
		return m.config.SettingsPath
	}
	root := m.config.UserStateRoot
	if root == "" {
		root = m.config.StateRoot
	}
	return filepath.Join(root, "settings.json")
}

func (m *Manager) notificationEnabled(kind string) bool {
	settings, err := state.LoadSettings(m.settingsPath())
	if err != nil {
		log.Printf("[push] cannot read notification settings: %v; suppressing %s notification; run `sessions notify status` after repairing the settings file", err, kind)
		return false
	}
	return settings.EffectiveNotify().Enabled(kind)
}

func (m *Manager) queueSessionNotification(id, kind string, payload PushPayload) {
	if id == "" || !m.notificationEnabled(kind) {
		return
	}
	now := time.Now()
	m.notificationMu.Lock()
	if m.notificationsClosed {
		m.notificationMu.Unlock()
		return
	}
	current := m.notifications[id]
	if current == nil {
		current = &sessionNotificationState{}
		m.notifications[id] = current
	}
	if current.lastSent.IsZero() || now.Sub(current.lastSent) >= m.options.NotifyCooldown {
		current.lastSent = now
		current.pending = nil
		if current.timer != nil {
			current.timer.Stop()
			current.timer = nil
		}
		m.notificationMu.Unlock()
		m.notify(payload)
		return
	}
	current.pending = &queuedSessionNotification{kind: kind, payload: payload}
	if current.timer == nil {
		delay := m.options.NotifyCooldown - now.Sub(current.lastSent)
		current.timer = time.AfterFunc(delay, func() { m.flushSessionNotification(id) })
	}
	m.notificationMu.Unlock()
}

func (m *Manager) flushSessionNotification(id string) {
	for {
		m.notificationMu.Lock()
		if m.notificationsClosed {
			m.notificationMu.Unlock()
			return
		}
		current := m.notifications[id]
		if current == nil || current.pending == nil {
			m.notificationMu.Unlock()
			return
		}
		pending := current.pending
		m.notificationMu.Unlock()

		enabled := m.notificationEnabled(pending.kind)
		m.notificationMu.Lock()
		if m.notificationsClosed {
			m.notificationMu.Unlock()
			return
		}
		current = m.notifications[id]
		if current == nil || current.pending != pending {
			m.notificationMu.Unlock()
			continue
		}
		current.pending = nil
		current.timer = nil
		if enabled {
			current.lastSent = time.Now()
		}
		m.notificationMu.Unlock()
		if enabled {
			m.notify(pending.payload)
		}
		return
	}
}

func (m *Manager) closeNotifications() {
	m.notificationMu.Lock()
	m.notificationsClosed = true
	for _, current := range m.notifications {
		if current.timer != nil {
			current.timer.Stop()
		}
	}
	m.notifications = make(map[string]*sessionNotificationState)
	m.notificationMu.Unlock()
}
