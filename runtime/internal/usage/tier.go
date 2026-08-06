package usage

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// A Codex event costs twice as much on the priority tier, so the tier is part
// of the price. The live recorder and the provider-log scanner write the same
// event_key, so they must agree on it or the recorded cost of a session would
// depend on which writer arrived first.
//
// The tier is a property of the session, never of the machine: two Codex
// sessions running side by side can be launched on different tiers. Evidence
// is therefore ranked, the strongest evidence any writer has seen decides the
// session's tier, and that decision is persisted so a rescan months later -
// after the runner metadata is gone or config.toml has been edited - reprices
// the session exactly as it was priced when it ran.
const (
	tierEvidenceNone    = 0
	tierEvidenceDefault = 1 // the provider-wide config.toml default
	tierEvidenceSession = 2 // this session's own launch arguments
)

type sessionTier struct {
	fast     bool
	evidence int
}

// codexTiers resolves and remembers the tier of every Codex conversation seen
// during one scan or one live event.
type codexTiers struct {
	db *sql.DB
	// fallback applies to sessions with no launch-argument evidence.
	fallback sessionTier
	// launched holds per-conversation evidence read from launch arguments.
	launched map[string]sessionTier
	resolved map[string]bool
}

func newCodexTiers(db *sql.DB, codexHome string, launched map[string]sessionTier) *codexTiers {
	return &codexTiers{
		db:       db,
		fallback: sessionTier{fast: fastServiceTier(codexConfigTier(codexHome)), evidence: tierEvidenceDefault},
		launched: launched,
		resolved: make(map[string]bool),
	}
}

// fast reports the pricing tier for one Codex conversation and records the
// decision so every later writer reaches the same answer.
func (t *codexTiers) fast(ctx context.Context, sessionID string) (bool, error) {
	if t == nil {
		return false, nil
	}
	if strings.TrimSpace(sessionID) == "" {
		// Without an identity there is nothing to agree about or persist; the
		// machine default is the best available answer.
		return t.fallback.fast, nil
	}
	if cached, ok := t.resolved[sessionID]; ok {
		return cached, nil
	}
	candidate := t.fallback
	if launch, ok := t.launched[sessionID]; ok {
		candidate = launch
	}
	stored, found, err := loadSessionTier(ctx, t.db, "codex", sessionID)
	if err != nil {
		return false, err
	}
	if found && stored.evidence >= candidate.evidence {
		// An already-decided tier only yields to strictly better evidence, so
		// editing config.toml later never rewrites what a session already cost.
		candidate = stored
	}
	if candidate.evidence > tierEvidenceNone && (!found || candidate.evidence > stored.evidence) {
		if err := saveSessionTier(ctx, t.db, "codex", sessionID, candidate); err != nil {
			return false, err
		}
	}
	t.resolved[sessionID] = candidate.fast
	return candidate.fast, nil
}

func loadSessionTier(ctx context.Context, db *sql.DB, provider, sessionID string) (sessionTier, bool, error) {
	var fast bool
	var evidence int
	err := db.QueryRowContext(ctx,
		`SELECT fast, evidence FROM usage_session_pricing WHERE provider = ? AND provider_session_id = ?`,
		provider, sessionID).Scan(&fast, &evidence)
	if err == sql.ErrNoRows {
		return sessionTier{}, false, nil
	}
	if err != nil {
		return sessionTier{}, false, err
	}
	return sessionTier{fast: fast, evidence: evidence}, true, nil
}

func saveSessionTier(ctx context.Context, db *sql.DB, provider, sessionID string, tier sessionTier) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO usage_session_pricing(provider, provider_session_id, fast, evidence) VALUES(?, ?, ?, ?)
ON CONFLICT(provider, provider_session_id) DO UPDATE SET fast=excluded.fast, evidence=excluded.evidence
WHERE excluded.evidence > usage_session_pricing.evidence`,
		provider, sessionID, tier.fast, tier.evidence)
	return err
}

// codexLaunchTiers reads the tier every locally launched Codex conversation
// asked for. It is the same evidence state.SessionInfo.Fast is derived from,
// which is what keeps the live path and the scanner in step.
func (s *Service) codexLaunchTiers() map[string]sessionTier {
	result := make(map[string]sessionTier)
	entries, err := os.ReadDir(s.options.RunnerStateDir)
	if err != nil {
		return result
	}
	for _, item := range entries {
		if item.IsDir() || !strings.HasSuffix(item.Name(), ".json") {
			continue
		}
		metadata, err := state.ReadRunnerMetadata(filepath.Join(s.options.RunnerStateDir, item.Name()))
		if err != nil || metadata.Info.ConversationID == "" {
			continue
		}
		if state.CommandTool(metadata.Info.Cmd) != state.ToolCodex {
			continue
		}
		home := metadata.ConfigDir
		if home == "" {
			home = s.defaultCodexHome()
		}
		if tier, ok := launchServiceTier(metadata.Info.Args, home); ok {
			result[metadata.Info.ConversationID] = sessionTier{fast: fastServiceTier(tier), evidence: tierEvidenceSession}
		}
	}
	return result
}

// codexTiersForSession builds the resolver for one live session.
func (s *Service) codexTiersForSession(db *sql.DB, info state.SessionInfo) *codexTiers {
	home := info.ConfigDir
	if home == "" {
		home = s.defaultCodexHome()
	}
	launched := make(map[string]sessionTier, 1)
	identity := info.ConversationID
	if tier, ok := launchServiceTier(info.Args, home); ok && identity != "" {
		launched[identity] = sessionTier{fast: fastServiceTier(tier), evidence: tierEvidenceSession}
	} else if ok {
		// No conversation identity yet: fold the session's own tier into the
		// fallback so this event is still priced from session evidence.
		return &codexTiers{db: db, fallback: sessionTier{fast: fastServiceTier(tier), evidence: tierEvidenceSession},
			launched: launched, resolved: make(map[string]bool)}
	} else if info.Fast && identity != "" {
		// The daemon resolved a premium tier some other way (for example an
		// app-server session). Treat it as session evidence too.
		launched[identity] = sessionTier{fast: true, evidence: tierEvidenceSession}
	}
	return newCodexTiers(db, home, launched)
}

func (s *Service) defaultCodexHome() string {
	for _, root := range s.options.CodexRoots {
		cleaned := strings.TrimSpace(root)
		if cleaned == "" {
			continue
		}
		return filepath.Dir(filepath.Clean(cleaned))
	}
	return ""
}

// launchServiceTier reads the tier a Codex session was launched with, either
// stated directly as `-c service_tier=...` or implied by `--profile`.
func launchServiceTier(args []string, configDir string) (string, bool) {
	profile := ""
	for index := 0; index+1 < len(args); index++ {
		switch args[index] {
		case "-c", "--config":
			value := args[index+1]
			if after, found := strings.CutPrefix(value, "service_tier="); found {
				return strings.Trim(strings.TrimSpace(after), `"'`), true
			}
			if after, found := strings.CutPrefix(value, "profile="); found {
				profile = strings.Trim(strings.TrimSpace(after), `"'`)
			}
		case "-p", "--profile":
			profile = strings.Trim(strings.TrimSpace(args[index+1]), `"'`)
		}
	}
	if profile == "" || configDir == "" {
		return "", false
	}
	// An unreadable or silent config is not evidence: say nothing rather than
	// claim the session ran on the standard tier.
	tier := codexConfigProfileTier(configDir, profile)
	return tier, tier != ""
}
