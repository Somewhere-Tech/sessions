package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

const (
	websocketReadLimit = 256 * 1024
	clientProtocol     = 2
)

type wsPeer struct {
	connection *websocket.Conn
	writes     sync.Mutex
}

func (p *wsPeer) send(ctx context.Context, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	p.writes.Lock()
	defer p.writes.Unlock()
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return p.connection.Write(writeCtx, websocket.MessageText, encoded)
}

// deniedWriteReason is sent to a socket that may observe sessions but has no
// authority to drive them. Keep it instructional: the client learns the frame
// was refused and how to become authorized instead of losing keystrokes.
const deniedWriteReason = "this connection may not send input; reconnect with a Sessions token"

// denyWrite answers a refused write frame, preferring the acknowledgement shape
// the caller is already waiting on so a pending request does not hang.
func denyWrite(ctx context.Context, peer *wsPeer, message clientMessage) {
	if message.RequestID != "" {
		switch message.Type {
		case "input", "submit":
			_ = peer.send(ctx, map[string]any{
				"type": message.Type + "Ack", "requestId": message.RequestID,
				"sessionId": message.SessionID, "ok": false, "error": deniedWriteReason,
			})
			return
		}
	}
	refusal := map[string]any{"type": "error", "code": "forbidden", "message": deniedWriteReason}
	addSessionID(refusal, message.SessionID)
	_ = peer.send(ctx, refusal)
}

func (s *Server) serveWebSocket(response http.ResponseWriter, request *http.Request, writes bool) {
	connection, err := websocket.Accept(response, request, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // Origin was checked against config.ts parity above.
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	connection.SetReadLimit(websocketReadLimit)
	peer := &wsPeer{connection: connection}
	if request.URL.Query().Get("mux") == "1" {
		s.handleMux(request.Context(), peer, writes)
		return
	}
	s.handleSingle(request.Context(), peer, request, writes)
}

func (s *Server) handleSingle(parent context.Context, peer *wsPeer, request *http.Request, writes bool) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer peer.connection.CloseNow()

	id := request.URL.Query().Get("sessionId")
	if id == "" {
		_ = peer.send(ctx, map[string]any{"type": "error", "message": "missing sessionId"})
		_ = peer.connection.Close(websocket.StatusPolicyViolation, "missing sessionId")
		return
	}
	session, ok := s.registry.Get(id)
	if !ok {
		if pending, paused := s.pendingRestore(id); paused {
			_ = peer.send(ctx, pendingRestoreSocketError(id, pending))
		} else {
			_ = peer.send(ctx, map[string]any{"type": "error", "message": "unknown session " + id})
		}
		_ = peer.connection.Close(websocket.StatusPolicyViolation, "unknown session")
		return
	}
	lastSeq := nonnegativeUint(request.URL.Query().Get("lastSeq"))
	claudeSince := int64(nonnegativeUint(request.URL.Query().Get("claudeEventsSince")))
	attachment := session.Attach(state.AttachOptions{
		LastSeq: lastSeq, ClaudeEventsSince: claudeSince,
		IncludeClaudeReplay: true, InitialReplayCap: 300,
	})
	defer attachment.Cancel()
	if err := sendInitial(ctx, peer, session, attachment, "", lastSeq, true); err != nil {
		return
	}
	if exited, terminal := session.TerminalState(); exited {
		_ = peer.send(ctx, exitMessage(terminal, ""))
		_ = peer.connection.Close(websocket.StatusNormalClosure, "pty exited")
		return
	}

	go streamAttachment(ctx, peer, attachment, streamOptions{
		includeOutput: true, includeClaudeLive: true,
		onExit: func() {
			_ = peer.connection.Close(websocket.StatusNormalClosure, "pty exited")
			cancel()
		},
		onUnavailable: func() {
			_ = peer.connection.Close(websocket.StatusServiceRestart, "runner reconnecting")
			cancel()
		},
	})
	for {
		messageType, payload, err := peer.connection.Read(ctx)
		if err != nil {
			return
		}
		// Binary frames and unparsed text frames are raw PTY input in this
		// mode, so they need the same write authority as an `input` message.
		if messageType == websocket.MessageBinary {
			if !writes {
				denyWrite(ctx, peer, clientMessage{Type: "input", SessionID: id})
				continue
			}
			s.registry.Input(ctx, id, string(payload))
			continue
		}
		var message clientMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			if !writes {
				denyWrite(ctx, peer, clientMessage{Type: "input", SessionID: id})
				continue
			}
			s.registry.Input(ctx, id, string(payload))
			continue
		}
		switch message.Type {
		case "ping":
			_ = peer.send(ctx, map[string]any{"type": "pong"})
		case "input":
			if !writes {
				denyWrite(ctx, peer, message)
				continue
			}
			s.registry.Input(ctx, id, message.Data)
		case "resize":
			if !writes {
				denyWrite(ctx, peer, message)
				continue
			}
			session.Resize(ctx, clampDimension(message.Cols, 40, 500), clampDimension(message.Rows, 10, 200))
		}
	}
}

type muxAttachment struct {
	cancel func()
}

func (s *Server) handleMux(parent context.Context, peer *wsPeer, writes bool) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer peer.connection.CloseNow()
	attached := make(map[string]muxAttachment)
	var attachedMu sync.Mutex
	detach := func(id string) {
		attachedMu.Lock()
		entry, ok := attached[id]
		if ok {
			delete(attached, id)
		}
		attachedMu.Unlock()
		if ok {
			entry.cancel()
		}
	}
	defer func() {
		attachedMu.Lock()
		entries := make([]muxAttachment, 0, len(attached))
		for _, entry := range attached {
			entries = append(entries, entry)
		}
		attached = make(map[string]muxAttachment)
		attachedMu.Unlock()
		for _, entry := range entries {
			entry.cancel()
		}
	}()

	for {
		messageType, payload, err := peer.connection.Read(ctx)
		if err != nil {
			return
		}
		if messageType != websocket.MessageText {
			continue
		}
		var message clientMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			continue
		}
		switch message.Type {
		case "ping":
			_ = peer.send(ctx, map[string]any{"type": "pong"})
		case "attach":
			if message.SessionID == "" {
				continue
			}
			attachedMu.Lock()
			_, exists := attached[message.SessionID]
			attachedMu.Unlock()
			if exists {
				continue
			}
			session, ok := s.registry.Get(message.SessionID)
			if !ok {
				if pending, paused := s.pendingRestore(message.SessionID); paused {
					_ = peer.send(ctx, pendingRestoreSocketError(message.SessionID, pending))
				} else {
					_ = peer.send(ctx, map[string]any{
						"type": "error", "message": "unknown session " + message.SessionID,
						"sessionId": message.SessionID,
					})
				}
				continue
			}
			includeOutput := message.OutputReplay == nil || *message.OutputReplay
			includeClaudeReplay := message.ClaudeReplay == nil || *message.ClaudeReplay
			includeClaudeLive := message.ClaudeLive == nil || *message.ClaudeLive
			attachment := session.Attach(state.AttachOptions{
				LastSeq: message.LastSeq, ClaudeEventsSince: message.ClaudeEventsSince,
				IncludeClaudeReplay: includeClaudeReplay, InitialReplayCap: 300,
			})
			attachedMu.Lock()
			attached[message.SessionID] = muxAttachment{cancel: attachment.Cancel}
			attachedMu.Unlock()
			if err := sendInitial(ctx, peer, session, attachment, message.SessionID, message.LastSeq, includeOutput); err != nil {
				detach(message.SessionID)
				continue
			}
			if exited, terminal := session.TerminalState(); exited {
				_ = peer.send(ctx, exitMessage(terminal, message.SessionID))
				detach(message.SessionID)
				continue
			}
			id := message.SessionID
			go streamAttachment(ctx, peer, attachment, streamOptions{
				sessionID: id, includeOutput: includeOutput, includeClaudeLive: includeClaudeLive,
				onExit: func() { detach(id) }, onUnavailable: func() { detach(id) },
			})
		case "detach":
			if message.SessionID != "" {
				detach(message.SessionID)
			}
		case "snapshot":
			s.handleMuxSnapshot(ctx, peer, message)
		case "events":
			s.handleMuxEvents(ctx, peer, message)
		case "input":
			if !writes {
				denyWrite(ctx, peer, message)
				continue
			}
			written := s.registry.Input(ctx, message.SessionID, message.Data)
			if message.RequestID != "" {
				_ = peer.send(ctx, map[string]any{
					"type": "inputAck", "requestId": message.RequestID,
					"sessionId": message.SessionID, "ok": written,
				})
			}
		case "submit":
			if !writes {
				denyWrite(ctx, peer, message)
				continue
			}
			// Same per-session lock the HTTP submit takes, so the two
			// transports cannot interleave a message and its Enter on one
			// session while leaving every other session free to run.
			unlock := s.submits.lock(message.SessionID)
			written := s.registry.Input(ctx, message.SessionID, message.Data)
			if written {
				timer := time.NewTimer(submitSettleDelay)
				select {
				case <-ctx.Done():
					timer.Stop()
					written = false
				case <-timer.C:
					written = s.registry.Input(ctx, message.SessionID, "\r")
				}
			}
			unlock()
			if message.RequestID != "" {
				_ = peer.send(ctx, map[string]any{
					"type": "submitAck", "requestId": message.RequestID,
					"sessionId": message.SessionID, "ok": written,
				})
			}
		case "resize":
			if !writes {
				denyWrite(ctx, peer, message)
				continue
			}
			if session, ok := s.registry.Get(message.SessionID); ok {
				session.Resize(ctx, clampDimension(message.Cols, 40, 500), clampDimension(message.Rows, 10, 200))
			}
		}
	}
}

type clientMessage struct {
	Type              string `json:"type"`
	Data              string `json:"data"`
	SessionID         string `json:"sessionId"`
	RequestID         string `json:"requestId"`
	Cols              int    `json:"cols"`
	Rows              int    `json:"rows"`
	LastSeq           uint32 `json:"lastSeq"`
	ClaudeEventsSince int64  `json:"claudeEventsSince"`
	Since             *int64 `json:"since"`
	Tail              *int64 `json:"tail"`
	Before            *int64 `json:"before"`
	OutputReplay      *bool  `json:"outputReplay"`
	ClaudeReplay      *bool  `json:"claudeReplay"`
	ClaudeLive        *bool  `json:"claudeLive"`
}

func sendInitial(
	ctx context.Context,
	peer *wsPeer,
	session *state.Session,
	attachment state.Attachment,
	sessionID string,
	lastSeq uint32,
	includeOutput bool,
) error {
	resumed := any(nil)
	if lastSeq > 0 {
		resumed = lastSeq
	}
	hello := map[string]any{
		"type": "hello", "protocol": clientProtocol, "session": session.Info(),
		"currentSeq": attachment.Replay.Current, "resumedFromSeq": resumed,
		"claudeEventsCount": attachment.ClaudeEventsCount,
		"claudeReplayStart": attachment.ClaudeReplayStart,
	}
	if sessionID != "" {
		hello["sessionId"] = sessionID
	}
	if err := peer.send(ctx, hello); err != nil {
		return err
	}
	if includeOutput {
		if attachment.Replay.Gap {
			message := map[string]any{
				"type": "gap", "oldestAvailableSeq": attachment.Replay.Oldest,
				"currentSeq": attachment.Replay.Current,
			}
			addSessionID(message, sessionID)
			if err := peer.send(ctx, message); err != nil {
				return err
			}
		}
		for _, event := range attachment.Replay.Events {
			message := map[string]any{"type": "output", "seq": event.Seq, "data": event.Data}
			addSessionID(message, sessionID)
			if err := peer.send(ctx, message); err != nil {
				return err
			}
		}
	}
	for _, event := range attachment.ClaudeEvents {
		message := map[string]any{"type": "claudeEvent", "event": json.RawMessage(event)}
		addSessionID(message, sessionID)
		if err := peer.send(ctx, message); err != nil {
			return err
		}
	}
	return nil
}

type streamOptions struct {
	sessionID         string
	includeOutput     bool
	includeClaudeLive bool
	onExit            func()
	onUnavailable     func()
}

func streamAttachment(ctx context.Context, peer *wsPeer, attachment state.Attachment, options streamOptions) {
	for event := range attachment.Events {
		var message map[string]any
		switch event.Kind {
		case proto.EventOutput:
			if !options.includeOutput || event.Output.Seq <= attachment.Replay.Current {
				continue
			}
			message = map[string]any{"type": "output", "seq": event.Output.Seq, "data": event.Output.Data}
		case proto.EventClaude:
			if !options.includeClaudeLive || event.ClaudeIndex < attachment.ClaudeEventsCount {
				continue
			}
			message = map[string]any{"type": "claudeEvent", "event": json.RawMessage(event.ClaudeEvent)}
		case proto.EventCodex:
			if !options.includeClaudeLive || event.ClaudeIndex < attachment.ClaudeEventsCount {
				continue
			}
			message = map[string]any{"type": "claudeEvent", "event": json.RawMessage(event.CodexEvent)}
		case proto.EventExit:
			message = exitMessage(event.Exit, options.sessionID)
			if err := peer.send(ctx, message); err == nil && options.onExit != nil {
				options.onExit()
			}
			return
		case proto.EventRunnerLost:
			message = unreachableMessage(event.Exit, options.sessionID)
			if err := peer.send(ctx, message); err == nil && options.onUnavailable != nil {
				options.onUnavailable()
			}
			return
		default:
			continue
		}
		addSessionID(message, options.sessionID)
		if err := peer.send(ctx, message); err != nil {
			return
		}
	}
}

func (s *Server) handleMuxSnapshot(ctx context.Context, peer *wsPeer, message clientMessage) {
	if message.RequestID == "" || message.SessionID == "" {
		return
	}
	session, ok := s.registry.Get(message.SessionID)
	if !ok {
		if pending, paused := s.pendingRestore(message.SessionID); paused {
			sendRPCError(ctx, peer, message.RequestID,
				pendingRestoreMessage(message.SessionID, pending), "SESSION_NEEDS_RECREATE", message.SessionID)
			return
		}
		sendRPCError(ctx, peer, message.RequestID, "unknown session "+message.SessionID, "not_found", message.SessionID)
		return
	}
	cols := message.Cols
	if cols < 0 {
		cols = 0
	}
	text, seq, err := session.Snapshot(ctx, cols)
	if err != nil {
		sendRPCError(ctx, peer, message.RequestID, err.Error(), "", message.SessionID)
		return
	}
	_ = peer.send(ctx, map[string]any{
		"type": "snapshot", "requestId": message.RequestID, "sessionId": message.SessionID,
		"text": text, "seq": seq,
	})
}

func (s *Server) handleMuxEvents(ctx context.Context, peer *wsPeer, message clientMessage) {
	if message.RequestID == "" || message.SessionID == "" {
		return
	}
	session, ok := s.registry.Get(message.SessionID)
	if !ok {
		if pending, paused := s.pendingRestore(message.SessionID); paused {
			sendRPCError(ctx, peer, message.RequestID,
				pendingRestoreMessage(message.SessionID, pending), "SESSION_NEEDS_RECREATE", message.SessionID)
			return
		}
		sendRPCError(ctx, peer, message.RequestID, "unknown session "+message.SessionID, "not_found", message.SessionID)
		return
	}
	body := s.eventsWindowBody(ctx, session, message.SessionID, message.Since, message.Tail, message.Before)
	body["type"] = "events"
	body["requestId"] = message.RequestID
	body["sessionId"] = message.SessionID
	_ = peer.send(ctx, body)
}

func pendingRestoreMessage(id string, pending state.RestorePending) string {
	reason := strings.TrimSpace(pending.Reason)
	if reason == "" {
		reason = "the runner stayed paused after reboot"
	}
	return "session is paused after reboot and cannot be read or controlled until it is resumed: " +
		reason + "; run sessions resume " + id
}

func pendingRestoreSocketError(id string, pending state.RestorePending) map[string]any {
	return map[string]any{
		"type": "error", "code": "SESSION_NEEDS_RECREATE", "sessionId": id,
		"message": pendingRestoreMessage(id, pending), "action": "sessions resume " + id,
	}
}

func sendRPCError(ctx context.Context, peer *wsPeer, requestID, message, code, sessionID string) {
	response := map[string]any{"type": "rpcError", "requestId": requestID, "message": message}
	if code != "" {
		response["code"] = code
	}
	if sessionID != "" {
		response["sessionId"] = sessionID
	}
	_ = peer.send(ctx, response)
}

func exitMessage(exit proto.ExitEvent, sessionID string) map[string]any {
	message := map[string]any{
		"type": "exit", "code": exit.Code, "signal": exit.Signal, "seq": exit.Seq,
	}
	if exit.Reason != "" {
		message["reason"] = exit.Reason
	}
	addSessionID(message, sessionID)
	return message
}

func unreachableMessage(lost proto.ExitEvent, sessionID string) map[string]any {
	reason := lost.Reason
	if reason == "" {
		reason = "runner-lost"
	}
	message := map[string]any{
		"type": "unreachable", "reason": reason, "seq": lost.Seq,
	}
	addSessionID(message, sessionID)
	return message
}

func addSessionID(message map[string]any, sessionID string) {
	if sessionID != "" {
		message["sessionId"] = sessionID
	}
}

func nonnegativeUint(raw string) uint32 {
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return 0
	}
	if value > float64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(value)
}

func clampDimension(value, minimum, maximum int) int {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}
