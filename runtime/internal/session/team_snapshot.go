package session

import (
	"context"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// TeamSnapshot is a strict listing for coordinators: failure to read the
// durable ledger must not produce a successful empty or incomplete team.
func (m *Manager) TeamSnapshot(ctx context.Context) ([]state.SessionInfo, error) {
	states, err := m.ledgerStates(ctx)
	if err != nil {
		return nil, err
	}
	infos := m.withDurableClosedStates(m.registry.List(true), states, true)
	infos = m.withPendingRestores(infos)
	infos = m.withRunnerReality(infos)
	infos = withLostReason(infos, states, bootAtMS())
	return m.withProvenanceStates(infos, states), nil
}
