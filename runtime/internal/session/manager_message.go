package session

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// SubmitMessage preserves the same principal/activity boundary as terminal
// input, but delivers a whole message and waits for the runner's decision.
func (m *Manager) SubmitMessage(ctx context.Context, id string, control proto.MessageControl, attribution state.InputAttribution) (proto.MessageResult, error) {
	var author ledger.MessageAuthor
	var err error
	if attribution.SourceSessionID != "" {
		if m.attributions == nil || id == attribution.SourceSessionID {
			return proto.MessageResult{}, errors.New("message attribution is unavailable or targets its source")
		}
		author, err = m.resolveMessageAuthor(ctx, attribution.SourceSessionID, attribution.Client)
		if err != nil {
			return proto.MessageResult{}, err
		}
	}
	current, ok := m.registry.Get(id)
	if !ok {
		return proto.MessageResult{}, &MessageInputUnavailableError{SessionID: id}
	}
	result, err := current.SubmitMessage(ctx, control)
	// A runner-reported unknown outcome means the text reached the provider
	// and may be in the conversation. Its history keeps the text, so its
	// authorship is recorded too; nothing else treats it as delivered.
	unknown := err == nil && !result.Accepted && result.Boundary == "unknown"
	if err != nil || (!result.Accepted && !unknown) {
		return result, err
	}
	if !unknown {
		principal, source := state.PrincipalHuman, ledger.ActivityHumanInput
		if attribution.SourceSessionID != "" {
			principal, source = state.PrincipalAgent, ledger.ActivitySessionInput
		}
		m.registry.RecordInputPrincipal(id, principal, control.Text)
		m.afterAcceptedInput(ctx, id, control.Text, source, result.Boundary != "queue")
	}
	if attribution.SourceSessionID != "" {
		exact, normalized := sha256.Sum256([]byte(control.Text)), sha256.Sum256([]byte(strings.TrimSpace(control.Text)))
		err = m.attributions.RecordMessageRelayed(ctx, ledger.MessageRelayed{
			Meta: ledger.Meta{LaneID: id}, Author: author,
			ContentSHA256: fmt.Sprintf("%x", exact[:]), ContentBytes: len([]byte(control.Text)),
			NormalizedSHA256: fmt.Sprintf("%x", normalized[:]), NormalizedBytes: len([]byte(strings.TrimSpace(control.Text))),
			OperationID: control.OperationID,
		})
		if err != nil {
			return result, &MessageAttributionCommitError{Err: err}
		}
	}
	return result, nil
}
