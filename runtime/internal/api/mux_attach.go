package api

import (
	"context"
	"sync"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

const maxMuxAttachments = 256

type muxAttachment struct{ cancel func() }

type muxAttachments struct {
	mu      sync.Mutex
	entries map[string]*muxAttachment
	closed  bool
}

func newMuxAttachments() *muxAttachments {
	return &muxAttachments{entries: make(map[string]*muxAttachment)}
}

// Reservations count before session lookup/replay so pending attaches and live
// streams share one atomic budget. Duplicate IDs consume no additional slot.
func (a *muxAttachments) reserve(id string) (slot *muxAttachment, full bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.entries[id] != nil {
		return nil, false
	}
	if len(a.entries) >= maxMuxAttachments {
		return nil, true
	}
	slot = &muxAttachment{}
	a.entries[id] = slot
	return slot, false
}

func (a *muxAttachments) install(id string, slot *muxAttachment, cancel func()) bool {
	a.mu.Lock()
	valid := !a.closed && a.entries[id] == slot
	if valid {
		slot.cancel = cancel
	}
	a.mu.Unlock()
	if !valid {
		cancel()
	}
	return valid
}

func (a *muxAttachments) detach(id string) {
	a.release(id, nil)
}

func (a *muxAttachments) release(id string, expected *muxAttachment) {
	a.mu.Lock()
	slot := a.entries[id]
	if expected != nil && slot != expected {
		a.mu.Unlock()
		return
	}
	delete(a.entries, id)
	var cancel func()
	if slot != nil {
		cancel = slot.cancel
	}
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *muxAttachments) close() {
	a.mu.Lock()
	var cancels []func()
	for _, slot := range a.entries {
		if slot.cancel != nil {
			cancels = append(cancels, slot.cancel)
		}
	}
	a.entries = make(map[string]*muxAttachment)
	a.closed = true
	a.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (s *Server) handleMuxAttach(ctx context.Context, peer *wsPeer, attached *muxAttachments, message clientMessage) {
	id := message.SessionID
	if id == "" {
		return
	}
	slot, full := attached.reserve(id)
	if full {
		_ = peer.send(ctx, map[string]any{
			"type": "error", "code": "mux_attachment_limit", "sessionId": id,
			"message": "Too many chats are streaming on this connection. Close an unused chat view and reopen this one. This view limit does not stop sessions.",
		})
		return
	}
	if slot == nil {
		return
	}
	session, ok := s.sessionOnContact(ctx, id)
	if !ok {
		attached.release(id, slot)
		if pending, paused := s.pendingRestore(id); paused {
			_ = peer.send(ctx, pendingRestoreSocketError(id, pending))
		} else {
			_ = peer.send(ctx, map[string]any{"type": "error", "message": "unknown session " + id, "sessionId": id})
		}
		return
	}
	includeOutput := message.OutputReplay == nil || *message.OutputReplay
	includeClaudeReplay := message.ClaudeReplay == nil || *message.ClaudeReplay
	includeClaudeLive := message.ClaudeLive == nil || *message.ClaudeLive
	attachment := session.Attach(state.AttachOptions{
		LastSeq: message.LastSeq, ClaudeEventsSince: message.ClaudeEventsSince,
		IncludeClaudeReplay: includeClaudeReplay, InitialReplayCap: 300,
	})
	if !attached.install(id, slot, attachment.Cancel) {
		return
	}
	if err := sendInitial(ctx, peer, session, attachment, id, message.LastSeq, includeOutput); err != nil {
		attached.release(id, slot)
		return
	}
	if exited, terminal := session.TerminalState(); exited {
		_ = peer.send(ctx, exitMessage(terminal, id))
		attached.release(id, slot)
		return
	}
	go streamAttachment(ctx, peer, attachment, streamOptions{
		sessionID: id, includeOutput: includeOutput, includeClaudeLive: includeClaudeLive,
		onExit: func() { attached.release(id, slot) }, onUnavailable: func() { attached.release(id, slot) },
	})
}
