package session

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"strings"
	"unicode"

	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func (m *Manager) Kill(ctx context.Context, id string, force bool) bool {
	return m.RequestKill(ctx, id, force) == nil
}

func (m *Manager) RequestKill(ctx context.Context, id string, force bool) error {
	return m.RequestKillAttributed(ctx, id, force, state.EndSessionRequest{})
}

// RequestKillAttributed records the authenticated initiator and optional
// operator-provided reason before the irreversible runner kill. Legacy callers
// may continue to use RequestKill; their records remain explicitly unattributed.
func (m *Manager) RequestKillAttributed(ctx context.Context, id string, force bool, end state.EndSessionRequest) error {
	var err error
	end, err = m.resolveEndInitiator(ctx, end)
	if err != nil {
		return err
	}
	if err := m.guard.Check(1, force); err != nil {
		return err
	}
	if _, ok := m.registry.Get(id); !ok && !m.lostWithoutRunner(ctx, id) {
		return fmt.Errorf("session %s not found", id)
	}
	return m.killOne(ctx, id, end)
}

func (m *Manager) KillMany(ctx context.Context, ids []string, force bool) error {
	return m.KillManyAttributed(ctx, ids, force, state.EndSessionRequest{})
}

func (m *Manager) KillManyAttributed(ctx context.Context, ids []string, force bool, end state.EndSessionRequest) error {
	var err error
	end, err = m.resolveEndInitiator(ctx, end)
	if err != nil {
		return err
	}
	unique := make(map[string]struct{})
	for _, id := range ids {
		if _, ok := m.registry.Get(id); ok {
			unique[id] = struct{}{}
		} else if m.lostWithoutRunner(ctx, id) {
			unique[id] = struct{}{}
		}
	}
	if err := m.guard.Check(len(unique), force); err != nil {
		return err
	}
	var failures []error
	for _, id := range sortedKeys(unique) {
		if err := m.killOne(ctx, id, end); err != nil {
			failures = append(failures, fmt.Errorf("kill session %s: %w", id, err))
		}
	}
	return errors.Join(failures...)
}

func (m *Manager) resolveEndInitiator(ctx context.Context, end state.EndSessionRequest) (state.EndSessionRequest, error) {
	if end.InitiatorKind == "" && end.InitiatorID == "" {
		return end, nil
	}
	kind := ledger.CreatorKind(end.InitiatorKind)
	if err := ledger.ValidateCreator(kind, end.InitiatorID); err != nil {
		return state.EndSessionRequest{}, fmt.Errorf("validate end initiator: %w", err)
	}
	if kind != ledger.CreatorSession {
		return end, nil
	}
	if m.ledgerReader == nil {
		return state.EndSessionRequest{}, errors.New("validate end initiator session: ledger reader is unavailable")
	}
	events, err := m.ledgerReader.Events(ctx, end.InitiatorID)
	if err != nil {
		return state.EndSessionRequest{}, fmt.Errorf("validate end initiator session: %w", err)
	}
	for _, candidate := range ledger.Fold(events) {
		if candidate.LaneID == end.InitiatorID && candidate.Created {
			if candidate.Archived {
				return state.EndSessionRequest{}, fmt.Errorf("end initiator session %s has been archived", end.InitiatorID)
			}
			end.InitiatorName = strings.TrimSpace(candidate.Name)
			if end.InitiatorName == "" {
				end.InitiatorName = strings.TrimSpace(candidate.Description)
			}
			return end, nil
		}
	}
	return state.EndSessionRequest{}, fmt.Errorf("end initiator session %s has no created event", end.InitiatorID)
}

func (m *Manager) killOne(ctx context.Context, id string, end state.EndSessionRequest) error {
	closeLostRecord := m.lostWithoutRunner(ctx, id)
	if m.boundaries != nil {
		if err := m.boundaries.RecordUserKill(ctx, ledger.UserKill{
			Meta:          ledger.Meta{LaneID: id},
			InitiatorKind: ledger.CreatorKind(end.InitiatorKind),
			InitiatorID:   end.InitiatorID,
			InitiatorName: end.InitiatorName,
			Client:        end.Client,
			Reason:        end.Reason,
			OperationID:   end.OperationID,
		}); err != nil {
			return fmt.Errorf("record user kill before runner kill: %w", err)
		}
	}
	if closeLostRecord {
		// There is no process to signal. Drop a stale unreachable connection if
		// one remains in memory; if discovery reattached it in the meantime,
		// fall through and deliver the normal kill control instead.
		if m.registry.RemoveUnreachable(id) {
			return nil
		}
		if _, live := m.registry.Get(id); !live {
			return nil
		}
	}
	return m.registry.RequestKill(ctx, id, true)
}

// lostWithoutRunner is the only case where an explicit end can close a record
// without sending runner control. The append-only ledger says contact was
// lost, and the same identity-aware process probe used by discovery confirms
// that this session's runner is not alive. A socket error alone is never
// enough: healthy runners can outlive a daemon connection.
func (m *Manager) lostWithoutRunner(ctx context.Context, id string) bool {
	if m.ledgerReader == nil || m.boundaries == nil || m.runtimeStillLive(id) {
		return false
	}
	lanes, err := m.ledgerStates(ctx)
	if err != nil {
		return false
	}
	for _, lane := range lanes {
		if lane.LaneID == id {
			return lane.Created && lane.RunnerLost && !lane.Archived && !durablyClosed(lane)
		}
	}
	return false
}

func (m *Manager) Input(ctx context.Context, id, data string) bool {
	if !m.registry.Input(ctx, id, data) {
		return false
	}
	// This is the unattributed door, and everything a person sends comes
	// through it: the HTTP input and submit routes, the WebSocket mux, an
	// attached terminal, `sessions send` typed by hand. Stamping here rather
	// than in each of those surfaces is what makes the answer complete by
	// construction, and it is deliberately not the transcript, because a
	// provider's own scheduled injections are written straight into the
	// transcript and never arrive here at all.
	m.registry.RecordInputPrincipal(id, state.PrincipalHuman, data)
	m.afterInput(ctx, id, data, ledger.ActivityHumanInput)
	return true
}

// InvalidMessageSourceError means the caller supplied lane provenance which
// cannot identify a durable, retained source lane.
type InvalidMessageSourceError struct{ Reason string }

func (e *InvalidMessageSourceError) Error() string { return e.Reason }

type InvalidAttributedInputError struct{ Reason string }

func (e *InvalidAttributedInputError) Error() string { return e.Reason }

// MessageInputUnavailableError means the target was not able to accept bytes.
type MessageInputUnavailableError struct{ SessionID string }

func (e *MessageInputUnavailableError) Error() string {
	return fmt.Sprintf("session %s is not available for input", e.SessionID)
}

// MessageAttributionCommitError is deliberately explicit: the provider
// accepted the bytes, so an automated caller must not retry and duplicate the
// turn, but the durable authorship fact could not be committed.
type MessageAttributionCommitError struct{ Err error }

func (e *MessageAttributionCommitError) Error() string {
	return fmt.Sprintf("message was delivered but its Sessions authorship record failed: %v; do not retry", e.Err)
}

func (e *MessageAttributionCommitError) Unwrap() error { return e.Err }

func (m *Manager) resolveMessageAuthor(ctx context.Context, sourceID, client string) (ledger.MessageAuthor, error) {
	if err := ledger.ValidateCreator(ledger.CreatorSession, sourceID); err != nil {
		return ledger.MessageAuthor{}, &InvalidMessageSourceError{Reason: err.Error()}
	}
	if m.ledgerReader == nil {
		return ledger.MessageAuthor{}, &InvalidMessageSourceError{Reason: "cannot validate source session: ledger reader is unavailable"}
	}
	events, err := m.ledgerReader.Events(ctx, sourceID)
	if err != nil {
		return ledger.MessageAuthor{}, &InvalidMessageSourceError{Reason: fmt.Sprintf("validate source session: %v", err)}
	}
	for _, candidate := range ledger.Fold(events) {
		if candidate.LaneID != sourceID || !candidate.Created {
			continue
		}
		if candidate.Archived {
			return ledger.MessageAuthor{}, &InvalidMessageSourceError{Reason: fmt.Sprintf("source session %s has been archived", sourceID)}
		}
		name := strings.TrimSpace(candidate.Name)
		if name == "" {
			name = "Unnamed session"
		}
		return ledger.MessageAuthor{
			Kind: ledger.CreatorSession, ID: sourceID, Name: name, Client: client,
		}, nil
	}
	return ledger.MessageAuthor{}, &InvalidMessageSourceError{Reason: fmt.Sprintf("source session %s has no created event", sourceID)}
}

// InputAttributed immediately delivers one text payload and records
// content-free lane authorship. The caller sends Enter separately without
// attribution so one provider turn produces exactly one relay fact.
func (m *Manager) InputAttributed(ctx context.Context, id, data string, attribution state.InputAttribution) error {
	if m.attributions == nil {
		return errors.New("message attribution is unavailable")
	}
	if strings.TrimSpace(data) == "" {
		return &InvalidAttributedInputError{Reason: "attributed input requires non-whitespace text"}
	}
	if id == attribution.SourceSessionID {
		return &InvalidMessageSourceError{Reason: "a session cannot relay a message to itself"}
	}
	author, err := m.resolveMessageAuthor(ctx, attribution.SourceSessionID, attribution.Client)
	if err != nil {
		return err
	}
	if !m.registry.Input(ctx, id, data) {
		return &MessageInputUnavailableError{SessionID: id}
	}
	// Attribution is what makes this an agent's message rather than a person's,
	// and the stamp is taken here rather than after the ledger commit below: the
	// provider already has the bytes, so the message happened whether or not the
	// authorship record does.
	m.registry.RecordInputPrincipal(id, state.PrincipalAgent, data)
	m.clearIdleAfterInput(id)
	exact := sha256.Sum256([]byte(data))
	normalizedText := strings.TrimSpace(data)
	normalized := sha256.Sum256([]byte(normalizedText))
	if err := m.attributions.RecordMessageRelayed(ctx, ledger.MessageRelayed{
		Meta: ledger.Meta{LaneID: id}, Author: author,
		ContentSHA256: fmt.Sprintf("%x", exact[:]), ContentBytes: len([]byte(data)),
		NormalizedSHA256: fmt.Sprintf("%x", normalized[:]), NormalizedBytes: len([]byte(normalizedText)),
	}); err != nil {
		return &MessageAttributionCommitError{Err: err}
	}
	m.afterInput(ctx, id, data, ledger.ActivitySessionInput)
	return nil
}

func (m *Manager) afterInput(ctx context.Context, id, data string, source ledger.ActivitySource) {
	m.clearIdleAfterInput(id)
	m.mu.Lock()
	runtime := m.runtimes[id]
	m.mu.Unlock()
	if runtime != nil {
		runtime.observeProviderInput(data)
	}
	if current, ok := m.registry.Get(id); ok && current.Info().SetAsideAt != nil {
		if _, err := m.registry.UpdateSetAside(id, false); err != nil {
			log.Printf("[working-set] bring back %s after input: %v", id, err)
		}
	}
	m.captureFirstMessageDescription(id, data)
	m.observe(ctx, "input activity", func(writer ledger.ObservationWriter) error {
		return writer.RecordActivity(ctx, ledger.Activity{
			Meta: ledger.Meta{LaneID: id}, Source: source,
		})
	})
}

const providerInputLimit = 1024 * 1024

func (r *runtimeSession) observeProviderInput(data string) {
	if r.session.Info().Tool != state.ToolCodex || data == "" {
		return
	}
	r.mu.Lock()
	for _, value := range []byte(data) {
		switch value {
		case '\r':
			prompt := normalizedTerminalPrompt(string(r.providerInput))
			r.providerInput = r.providerInput[:0]
			watcher := r.watcher
			r.mu.Unlock()
			if watcher != nil && prompt != "" {
				watcher.ExpectInput(prompt)
			}
			return
		case 0x7f:
			if len(r.providerInput) > 0 {
				r.providerInput = r.providerInput[:len(r.providerInput)-1]
			}
		default:
			if len(r.providerInput) < providerInputLimit {
				r.providerInput = append(r.providerInput, value)
			}
		}
	}
	r.mu.Unlock()
}

func normalizedTerminalPrompt(value string) string {
	value = strings.ReplaceAll(value, "\x1b[200~", "")
	value = strings.ReplaceAll(value, "\x1b[201~", "")
	return strings.TrimSpace(value)
}

func (m *Manager) clearIdleAfterInput(id string) {
	if current, ok := m.registry.Get(id); ok {
		current.ClearIdleResult()
	}
}

// MessageRelays returns the content-free attribution facts for one target
// lane. Correlation with provider transcript text happens only in response
// memory and never writes provider history.
func (m *Manager) MessageRelays(ctx context.Context, id string) ([]ledger.MessageRelayed, error) {
	if m.ledgerReader == nil {
		return nil, errors.New("message attribution is unavailable")
	}
	events, err := m.ledgerReader.Events(ctx, id)
	if err != nil {
		return nil, err
	}
	relays := make([]ledger.MessageRelayed, 0)
	for _, event := range events {
		if event.Type != ledger.EventMessageRelayed {
			continue
		}
		relay, err := ledger.DecodeMessageRelayed(event)
		if err != nil {
			return nil, err
		}
		relays = append(relays, relay)
	}
	return relays, nil
}

func (m *Manager) ConfigureModel(ctx context.Context, id, model, effort string) (state.SessionInfo, error) {
	current, ok := m.registry.Get(id)
	if !ok {
		return state.SessionInfo{}, fmt.Errorf("%w: session %s", state.ErrSessionNotFound, id)
	}
	info := current.Info()
	if info.Kind != state.KindCodexAppServer && info.Kind != state.KindClaudeStructured {
		return state.SessionInfo{}, errors.New("model changes are available only for Rich Claude and Rich Codex sessions; Terminal sessions keep their provider's own controls")
	}
	if info.Working {
		return state.SessionInfo{}, fmt.Errorf("%w; wait for the turn to finish with `sessions wait %s`", state.ErrSessionWorking, id)
	}
	model = strings.TrimSpace(model)
	effort = strings.ToLower(strings.TrimSpace(effort))
	if model == "" {
		return state.SessionInfo{}, errors.New("model is required")
	}

	if info.Kind == state.KindCodexAppServer {
		choice := codexModelChoice(info.Args)
		choice.Model = model
		choice.Effort = effort
		catalog, err := m.listModels(ctx, info.Cmd)
		if err != nil {
			return state.SessionInfo{}, fmt.Errorf("load live Codex model catalog: %w", err)
		}
		resolved, err := codexapp.ResolveModelChoice(catalog, choice)
		if err != nil {
			return state.SessionInfo{}, err
		}
		model = resolved.Model
		effort = resolved.Effort
	} else {
		if len(model) > 128 || strings.ContainsAny(model, "\r\n\x00") {
			return state.SessionInfo{}, errors.New("invalid Claude model")
		}
		switch effort {
		case "", "low", "medium", "high", "xhigh", "max":
		default:
			return state.SessionInfo{}, errors.New("Claude effort must be low, medium, high, xhigh, max, or empty")
		}
	}

	return m.registry.ConfigureModel(ctx, id, model, effort)
}

func (m *Manager) ModelOptions(ctx context.Context, id string) ([]codexapp.Model, error) {
	current, ok := m.registry.Get(id)
	if !ok {
		return nil, fmt.Errorf("%w: session %s", state.ErrSessionNotFound, id)
	}
	info := current.Info()
	if info.Kind != state.KindCodexAppServer {
		return nil, errors.New("the live model catalog is available for Rich Codex sessions")
	}
	return m.listModels(ctx, info.Cmd)
}

// CodexModelOptions returns the same live provider catalog used to validate
// Rich Codex sessions, but does not require a session to exist yet. The native
// launcher uses it to present real choices before creating a runtime.
func (m *Manager) CodexModelOptions(ctx context.Context) ([]codexapp.Model, error) {
	return m.listModels(ctx, "codex")
}

func (m *Manager) captureFirstMessageDescription(id, data string) {
	m.mu.Lock()
	runtime := m.runtimes[id]
	m.mu.Unlock()
	if runtime == nil {
		return
	}

	runtime.mu.Lock()
	if runtime.firstMessageDone {
		runtime.mu.Unlock()
		return
	}
	info := runtime.session.Info()
	if info.DescriptionSource == state.DescriptionExplicit || info.Description != "" {
		runtime.firstMessageDone = true
		runtime.mu.Unlock()
		return
	}

	complete := false
	for _, value := range []byte(data) {
		if value == '\r' || (value == '\n' && len(data) == 1) {
			complete = len(runtime.firstMessageInput) > 0
			if complete {
				break
			}
			continue
		}
		if len(runtime.firstMessageInput) < 4096 {
			runtime.firstMessageInput = append(runtime.firstMessageInput, value)
		}
	}
	if !complete {
		runtime.mu.Unlock()
		return
	}
	description := firstMessageDescription(string(runtime.firstMessageInput))
	if description == "" {
		runtime.firstMessageInput = nil
		runtime.mu.Unlock()
		return
	}
	runtime.firstMessageDone = true
	runtime.firstMessageInput = nil
	runtime.mu.Unlock()

	changed, err := m.registry.SetFirstMessageDescription(id, description)
	if err != nil {
		log.Printf("[description] persist first-message description for %s: %v", id, err)
	}
	if !changed {
		return
	}
	m.observe(context.Background(), "derived description", func(writer ledger.ObservationWriter) error {
		return writer.RecordDescriptionDerived(context.Background(), ledger.DescriptionDerived{
			Meta: ledger.Meta{LaneID: id}, Description: description, Source: ledger.DescriptionFirstMessage,
		})
	})
}

func firstMessageDescription(value string) string {
	var cleaned strings.Builder
	escapeSequence := 0
	for _, character := range value {
		if escapeSequence != 0 {
			if escapeSequence == 1 {
				if character == '[' {
					escapeSequence = 2
				} else {
					escapeSequence = 0
				}
			} else if character >= '@' && character <= '~' {
				escapeSequence = 0
			}
			continue
		}
		if character == '\x1b' {
			escapeSequence = 1
			continue
		}
		if unicode.IsControl(character) {
			cleaned.WriteRune(' ')
			continue
		}
		cleaned.WriteRune(character)
	}
	description := strings.Join(strings.Fields(cleaned.String()), " ")
	runes := []rune(description)
	if len(runes) > 80 {
		description = string(runes[:79]) + "…"
	}
	return description
}
