package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
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

func (s *Server) serveWebSocket(response http.ResponseWriter, request *http.Request) {
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
		s.handleMux(request.Context(), peer)
		return
	}
	s.handleSingle(request.Context(), peer, request)
}

func (s *Server) handleSingle(parent context.Context, peer *wsPeer, request *http.Request) {
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
		_ = peer.send(ctx, map[string]any{"type": "error", "message": "unknown session " + id})
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
	})
	for {
		messageType, payload, err := peer.connection.Read(ctx)
		if err != nil {
			return
		}
		if messageType == websocket.MessageBinary {
			s.registry.Input(ctx, id, string(payload))
			continue
		}
		var message clientMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			s.registry.Input(ctx, id, string(payload))
			continue
		}
		switch message.Type {
		case "ping":
			_ = peer.send(ctx, map[string]any{"type": "pong"})
		case "input":
			s.registry.Input(ctx, id, message.Data)
		case "resize":
			session.Resize(ctx, clampDimension(message.Cols, 40, 500), clampDimension(message.Rows, 10, 200))
		}
	}
}

type muxAttachment struct {
	cancel func()
}

func (s *Server) handleMux(parent context.Context, peer *wsPeer) {
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
				_ = peer.send(ctx, map[string]any{
					"type": "error", "message": "unknown session " + message.SessionID,
					"sessionId": message.SessionID,
				})
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
				onExit: func() { detach(id) },
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
			written := s.registry.Input(ctx, message.SessionID, message.Data)
			if message.RequestID != "" {
				_ = peer.send(ctx, map[string]any{
					"type": "inputAck", "requestId": message.RequestID,
					"sessionId": message.SessionID, "ok": written,
				})
			}
		case "resize":
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
		case proto.EventExit, proto.EventRunnerLost:
			message = exitMessage(event.Exit, options.sessionID)
			if err := peer.send(ctx, message); err == nil && options.onExit != nil {
				options.onExit()
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
		sendRPCError(ctx, peer, message.RequestID, "unknown session "+message.SessionID, "not_found", message.SessionID)
		return
	}
	window := session.EventsWindow(message.Since, message.Tail, nil)
	_ = peer.send(ctx, map[string]any{
		"type": "events", "requestId": message.RequestID, "sessionId": message.SessionID,
		"events": window.Events, "nextIndex": window.NextIndex, "totalCount": window.TotalCount,
	})
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
