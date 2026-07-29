package usage

import (
	"context"
	"sort"
	"strings"
	"time"
)

// ObservedSessions returns the provider conversations and physical logs
// already indexed by the most recent Report/Sync call for the requested
// window. It never reads transcript contents.
func (s *Service) ObservedSessions(ctx context.Context, since, until time.Time) ([]ObservedSession, error) {
	db, err := s.database(ctx)
	if err != nil {
		return nil, err
	}
	query := `SELECT provider, provider_session_id, source_path, event_key, timestamp_ms
FROM usage_entries WHERE timestamp_ms >= ? AND timestamp_ms < ?
ORDER BY timestamp_ms ASC`
	rows, err := db.QueryContext(ctx, query, since.UnixMilli(), until.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byIdentity := make(map[string]*ObservedSession)
	paths := make(map[string]map[string]struct{})
	turns := make(map[string]map[string]struct{})
	for rows.Next() {
		var provider, providerSessionID, sourcePath, eventKey string
		var timestamp int64
		if err := rows.Scan(&provider, &providerSessionID, &sourcePath, &eventKey, &timestamp); err != nil {
			return nil, err
		}
		if strings.TrimSpace(providerSessionID) == "" {
			continue
		}
		key := provider + ":" + providerSessionID
		current := byIdentity[key]
		if current == nil {
			current = &ObservedSession{
				Provider: provider, ProviderSessionID: providerSessionID,
				FirstActivityAt: timestamp, LastActivityAt: timestamp,
			}
			byIdentity[key] = current
			paths[key] = make(map[string]struct{})
			turns[key] = make(map[string]struct{})
		}
		if timestamp < current.FirstActivityAt {
			current.FirstActivityAt = timestamp
		}
		if timestamp > current.LastActivityAt {
			current.LastActivityAt = timestamp
		}
		current.Entries++
		if sourcePath != "" && !strings.HasPrefix(sourcePath, "live://") {
			paths[key][sourcePath] = struct{}{}
		}
		prefix := "codex:" + providerSessionID + ":"
		if provider == "codex" && strings.HasPrefix(eventKey, prefix) {
			turns[key][strings.TrimPrefix(eventKey, prefix)] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	result := make([]ObservedSession, 0, len(byIdentity))
	for key, current := range byIdentity {
		for path := range paths[key] {
			current.SourcePaths = append(current.SourcePaths, path)
		}
		sort.Strings(current.SourcePaths)
		for turn := range turns[key] {
			current.TurnIDs = append(current.TurnIDs, turn)
		}
		sort.Strings(current.TurnIDs)
		result = append(result, *current)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].LastActivityAt != result[j].LastActivityAt {
			return result[i].LastActivityAt < result[j].LastActivityAt
		}
		if result[i].Provider != result[j].Provider {
			return result[i].Provider < result[j].Provider
		}
		return result[i].ProviderSessionID < result[j].ProviderSessionID
	})
	return result, nil
}
