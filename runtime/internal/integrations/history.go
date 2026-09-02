// Package integrations owns the stable, versioned contracts consumed by
// external Sessions integrations. It does not call any external service.
package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/backup"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

const SchemaVersion = 1
const MaxTranscriptWindowSpan = 500

var ErrHistoryNotFound = errors.New("history session not found")
var ErrHistoryChanged = errors.New("history changed since search")

type HistorySession struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Tool              string `json:"tool"`
	ProviderSessionID string `json:"provider_session_id,omitempty"`
	CWD               string `json:"cwd"`
	Machine           string `json:"machine"`
	CreatorKind       string `json:"creator_kind,omitempty"`
	CreatorID         string `json:"creator_id,omitempty"`
	CreatedAt         int64  `json:"created_at"`
	LastActivityAt    int64  `json:"last_activity_at"`
	// ConversationUpdatedAt is when the conversation itself was last written
	// to, taken from the transcript the conversation lives in. It is not the
	// same question as LastActivityAt, which also counts activity on the
	// Sessions record — a runner draining its terminal at shutdown, or the
	// daemon rewriting its own bookkeeping, moves LastActivityAt without a
	// single word being said. Ordering conversations by recency needs the
	// former: otherwise a batch of long-finished sessions all touched by one
	// housekeeping pass outranks the conversation the user was actually in.
	// Zero when no transcript backs the record, in which case LastActivityAt
	// remains the only answer available.
	ConversationUpdatedAt int64 `json:"conversation_updated_at,omitempty"`
	// ConversationUpdatedApproximate marks a ConversationUpdatedAt that is the
	// transcript file's modification time rather than the conversation's own
	// last record. mtime is metadata about the file, not about the conversation:
	// any copy that does not preserve times -- a plain `cp -R`, an rsync without
	// -t, a restore, a move to another machine -- resets it, and every
	// conversation then reads as having just happened.
	//
	// The flag is positive on purpose. It is set only by a daemon that looked
	// and found nothing stamped, so false means either "taken from the records"
	// or "this daemon is too old to have looked", and a client that cannot tell
	// those apart must not brand a row approximate on a guess. Branding every
	// row of an older daemon's answer would be the same fabrication in the
	// opposite direction from the one this field exists to stop.
	ConversationUpdatedApproximate bool `json:"conversation_updated_approximate,omitempty"`
	MessageCount                   int  `json:"message_count"`
	// MessageCountUncounted marks a MessageCount that is not a count. The
	// summary listing does not parse transcripts, so a row it could not answer
	// for carries zero — and zero is also what a genuinely empty conversation
	// carries. A consumer filtering out empty conversations cannot tell those
	// apart, which is exactly why the app's conversation browser had to abandon
	// the cheap view and pay for the exact one on first paint.
	//
	// Positive on purpose, like ConversationUpdatedApproximate: it is set only
	// by a daemon that deliberately declined to count, so false means "this is a
	// real count" or "this daemon is too old to say", and neither is a licence
	// to treat a real zero as unknown.
	MessageCountUncounted bool   `json:"message_count_uncounted,omitempty"`
	ConversationAvailable bool   `json:"conversation_available"`
	External              bool   `json:"external,omitempty"`
	PromptHistoryOnly     bool   `json:"prompt_history_only,omitempty"`
	ReopenedAs            string `json:"reopened_as,omitempty"`
	ResumedFrom           string `json:"resumed_from,omitempty"`
	MovedToEndpoint       string `json:"moved_to_endpoint,omitempty"`
	MovedToSessionID      string `json:"moved_to_session_id,omitempty"`
	MovedFromEndpoint     string `json:"moved_from_endpoint,omitempty"`
	MovedFromSessionID    string `json:"moved_from_session_id,omitempty"`
	SourceFingerprint     string `json:"-"`
	// Unreadable marks a session whose transcript could not be read on this
	// pass. The session itself is still listed, named, and addressable: losing
	// one file must never make a session unfindable, and must never remove the
	// other sessions from the list.
	Unreadable bool `json:"unreadable,omitempty"`
	// UnreadableReason states what failed and what to do about it.
	UnreadableReason string `json:"unreadable_reason,omitempty"`
	// SkippedRecords counts torn or undecodable records skipped inside this
	// session's transcript, so a degraded message count is never mistaken for
	// an exact one.
	SkippedRecords int `json:"skipped_records,omitempty"`

	// Surface says where the conversation was started from -- Codex Desktop,
	// the Codex CLI, a headless `codex exec`, Claude Code, Claude Desktop, the
	// Agent SDK, or Sessions itself -- and whether a person or an agent drove
	// it. Neither provider's own picker can answer this, which is the reason it
	// is carried here at all.
	//
	// Additive and optional in both directions. Nil means the provider recorded
	// nothing this reader understood, and is deliberately distinguishable from
	// a surface whose fields happen to be empty; an older record that predates
	// the field decodes to nil rather than failing, and a daemon too old to send
	// it produces nil on the client rather than a wrong answer.
	Surface *watch.ConversationSurface `json:"surface,omitempty"`
}

type HistoryResponse struct {
	SchemaVersion int              `json:"schemaVersion"`
	Sessions      []HistorySession `json:"sessions"`
	// UnreadableSessions and SkippedRecords aggregate the per-session
	// degradation above, so a client can tell a complete list from a partial
	// one without inspecting every entry.
	UnreadableSessions int `json:"unreadable_sessions,omitempty"`
	SkippedRecords     int `json:"skipped_records,omitempty"`
}

type HistorySource struct {
	SchemaVersion int            `json:"schemaVersion"`
	Session       HistorySession `json:"session"`
	SourceKind    string         `json:"source_kind"`
	SourcePath    string         `json:"source_path,omitempty"`
	RawBytes      int64          `json:"raw_bytes,omitempty"`
	RawAvailable  bool           `json:"raw_available"`
	TextAvailable bool           `json:"text_available"`
	// MirrorDamaged reports that this conversation is being served from
	// Sessions' own copy and that the copy records having stopped storing
	// provider records. Reading a known-incomplete conversation is exactly the
	// moment the reader needs telling, so the fact travels with the source
	// rather than staying in the sidecar nobody reads.
	//
	// Both fields are absent when the source is not a mirror, and absent when
	// the mirror's health is unknown -- a mirror with no readable sidecar is
	// not thereby a damaged one.
	MirrorDamaged bool   `json:"mirror_damaged,omitempty"`
	MirrorDetail  string `json:"mirror_detail,omitempty"`
}

type TranscriptMessage struct {
	Index     int            `json:"index"`
	ID        string         `json:"id"`
	Role      string         `json:"role"`
	Kind      string         `json:"kind,omitempty"`
	Text      string         `json:"text"`
	Timestamp *string        `json:"timestamp"`
	Author    *MessageAuthor `json:"author,omitempty"`
}

type MessageAuthor struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Name   string `json:"name"`
	Client string `json:"client"`
}

type TranscriptResponse struct {
	SchemaVersion int                 `json:"schemaVersion"`
	Session       HistorySession      `json:"session"`
	Messages      []TranscriptMessage `json:"messages"`
	Truncated     bool                `json:"truncated,omitempty"`
	HasMore       bool                `json:"has_more,omitempty"`
	NextIndex     int                 `json:"next_index,omitempty"`
	// SkippedRecords counts torn or undecodable records skipped while reading
	// this transcript. Nonzero means the conversation is shown minus those
	// records, not that the read failed.
	SkippedRecords int `json:"skipped_records,omitempty"`
}

type TranscriptWindowOptions struct {
	Start           int
	End             int
	Role            string
	ExpectedIndex   int
	ExpectedMessage string
}

type HistoryOptions struct {
	RunnerStateDir          string
	ClaudeProjectsDir       string
	ClaudeHistoryPath       string
	CodexSessionsDir        string
	Machine                 string
	Now                     func() time.Time
	DiscoverProviderHistory bool
}

// messageCountMode decides how much a listing is willing to pay for message
// counts. Counting a transcript means parsing all of it — a count is not
// derivable from a line count — and on the development machine's real history
// that is 1.4 GB across 303 conversations and seven seconds of cold wall clock,
// against 0.15 seconds for the same listing without it. The cheap view must
// stay cheap, and the expensive view must stay exact, so the choice is made by
// the caller rather than by a heuristic inside the store.
type messageCountMode int

const (
	// countNone answers a single-session lookup, where the count is about to be
	// recomputed from the messages actually returned.
	countNone messageCountMode = iota

	// countCached reports the counts already in the cache and does not parse a
	// single byte for the rest. This is the summary listing: it costs a map
	// lookup per row, and it is honest about which rows it could not answer for
	// rather than reporting them as empty.
	countCached

	// countAll parses whatever is not cached. This is the exact listing.
	countAll
)

// countMessages applies the mode. The third result says whether the returned
// count is a count at all, which is the distinction the summary view previously
// could not express: a transcript nobody counted and a transcript with nothing
// in it both arrived as zero, and a consumer filtering out empty conversations
// therefore had to abandon the cheap view entirely to avoid hiding everything.
func (h *HistoryStore) countMessages(
	mode messageCountMode, path, tool string, info os.FileInfo,
) (count, skipped int, counted bool, err error) {
	switch mode {
	case countAll:
		count, skipped, err = h.messageCount(path, tool, info)
		return count, skipped, err == nil, err
	case countCached:
		count, skipped, ok := h.cachedMessageCount(path, info)
		if !ok {
			// Nothing is parsed in this mode, but a transcript that cannot be
			// opened must still degrade its row: a Resume list that quietly
			// drops an unreadable conversation looks identical to one where
			// the conversation never existed. One open, no read.
			file, err := os.Open(path)
			if err != nil {
				return 0, 0, false, err
			}
			_ = file.Close()
		}
		return count, skipped, ok, nil
	default:
		return 0, 0, false, nil
	}
}

// cachedMessageCount returns a count already computed for this exact file, at
// no I/O cost. The cache is keyed by path, size and modification time, so a
// transcript that has grown since it was counted is a miss rather than a stale
// answer.
func (h *HistoryStore) cachedMessageCount(path string, info os.FileInfo) (int, int, bool) {
	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()
	cached, ok := h.cache[path]
	if !ok || cached.size != info.Size() || cached.modTimeNano != info.ModTime().UnixNano() {
		return 0, 0, false
	}
	h.cacheClock++
	cached.used = h.cacheClock
	h.cache[path] = cached
	return cached.count, cached.skipped, true
}

// maxHistoryCacheEntries bounds the path-keyed message-count cache. The cache
// is keyed by transcript path, and a long-lived daemon sees an unbounded number
// of paths over its lifetime, so it is evicted rather than grown forever.
const maxHistoryCacheEntries = 4096

type historyCacheEntry struct {
	size        int64
	modTimeNano int64
	count       int
	skipped     int
	used        uint64
}

type HistoryStore struct {
	options          HistoryOptions
	cacheMu          sync.Mutex
	cacheClock       uint64
	cache            map[string]historyCacheEntry
	providerMu       sync.Mutex
	providerCachedAt time.Time
	providerCache    []watch.ResumableSession
	archiveCachedAt  time.Time
	archiveCache     []watch.ArchivedClaudeConversation
}

func NewHistoryStore(options HistoryOptions) *HistoryStore {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.ClaudeHistoryPath == "" && options.ClaudeProjectsDir != "" {
		options.ClaudeHistoryPath = filepath.Join(filepath.Dir(options.ClaudeProjectsDir), "history.jsonl")
	}
	return &HistoryStore{options: options, cache: make(map[string]historyCacheEntry)}
}

func (h *HistoryStore) List(live []state.SessionInfo) (HistoryResponse, error) {
	sessions, err := h.list(live, countAll)
	if err != nil {
		return HistoryResponse{}, err
	}
	response := HistoryResponse{SchemaVersion: SchemaVersion, Sessions: sessions}
	for _, session := range sessions {
		if session.Unreadable {
			response.UnreadableSessions++
		}
		response.SkippedRecords += session.SkippedRecords
	}
	return response, nil
}

// SearchSessions returns the known history sources without parsing every
// transcript just to count its messages. Search reads only candidates that
// survive its session/tool filters and applies its own bounded transcript read.
func (h *HistoryStore) SearchSessions(live []state.SessionInfo) ([]HistorySession, error) {
	return h.list(live, countCached)
}

// Lookup resolves one exact history identifier through the same provider and
// prompt-index discovery boundary used by History and Search. Callers use this
// instead of reconstructing provider identity or cwd from client-supplied
// fields.
func (h *HistoryStore) Lookup(live []state.SessionInfo, id string) (HistorySession, error) {
	source, ok := h.find(live, strings.TrimSpace(id))
	if !ok {
		return HistorySession{}, ErrHistoryNotFound
	}
	if source.archived != nil {
		return h.describeArchived(*source.archived), nil
	}
	session, _, _, err := h.describeSource(source, countNone)
	return session, err
}

// Source describes the provider-owned source behind one Sessions history
// record. The authenticated endpoint is deliberately separate from History so
// routine lists do not disclose local paths. It is read-only.
func (h *HistoryStore) Source(live []state.SessionInfo, id string) (HistorySource, error) {
	source, ok := h.find(live, strings.TrimSpace(id))
	if !ok {
		return HistorySource{}, ErrHistoryNotFound
	}
	if source.archived != nil {
		session := h.describeArchived(*source.archived)
		return HistorySource{
			SchemaVersion: SchemaVersion, Session: session,
			SourceKind: "prompt-index", SourcePath: h.options.ClaudeHistoryPath,
			RawAvailable: false, TextAvailable: session.ConversationAvailable,
		}, nil
	}
	session, path, _, err := h.describeSource(source, countNone)
	if err != nil {
		return HistorySource{}, err
	}
	result := HistorySource{
		SchemaVersion: SchemaVersion, Session: session,
		SourceKind: "provider-jsonl", SourcePath: path,
		RawAvailable: path != "", TextAvailable: session.ConversationAvailable,
	}
	if path == "" {
		result.SourceKind = "missing"
		return result, nil
	}
	// Saying "provider-jsonl" when the provider no longer has the file would
	// imply the conversation is still recoverable through the provider's own
	// resume. It is not: this is the copy Sessions kept.
	if source.managed != nil &&
		watch.TranscriptMirrorPath(h.options.RunnerStateDir, source.managed.ID) == path {
		result.SourceKind = string(watch.ClaudeMirror)
		// "sessions-mirror" already says the provider's file is gone. It does
		// not say whether the copy that replaced it is the whole conversation,
		// and that is the one thing a reader cannot find out for themselves.
		if health := watch.ReadTranscriptMirrorHealth(path); health.Degraded() {
			result.MirrorDamaged = true
			result.MirrorDetail = health.Detail()
		}
	}
	if info, statErr := os.Stat(path); statErr == nil && info.Mode().IsRegular() {
		result.RawBytes = info.Size()
	}
	return result, nil
}

func (h *HistoryStore) list(live []state.SessionInfo, counting messageCountMode) ([]HistorySession, error) {
	sources := backup.CollectSessions(live, h.options.RunnerStateDir)
	sessions := make([]HistorySession, 0, len(sources))
	managedByProvider := make(map[string]int, len(sources))
	for _, source := range sources {
		// Torn-record policy, applied per item: one unreadable transcript
		// degrades that one row. Returning the error here emptied the whole
		// history UI, so a single JSONL on a stale mount cost the user the
		// ability to browse every other session they own.
		session, _, _, err := h.describe(source, counting)
		if err != nil {
			session = markUnreadable(session, source.ID, err)
		}
		if session.ProviderSessionID != "" {
			managedByProvider[historyProviderKey(session.Tool, session.ProviderSessionID)] = len(sessions)
		}
		sessions = append(sessions, session)
	}
	if h.options.DiscoverProviderHistory {
		for _, source := range h.providerConversations() {
			key := historyProviderKey(source.Tool, source.SessionID)
			if index, exists := managedByProvider[key]; exists {
				if title := compactHistoryTitle(source.Title); title != "" {
					sessions[index].Name = title
				}
				continue
			}
			session, _, _, err := h.describeExternal(source, counting)
			if err != nil {
				session = markUnreadable(session, source.SessionID, err)
			}
			sessions = append(sessions, session)
		}
		for _, source := range h.archivedClaudeConversations() {
			key := historyProviderKey("claude", source.SessionID)
			if _, exists := managedByProvider[key]; exists {
				continue
			}
			sessions = append(sessions, h.describeArchived(source))
		}
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].LastActivityAt != sessions[j].LastActivityAt {
			return sessions[i].LastActivityAt > sessions[j].LastActivityAt
		}
		return sessions[i].ID < sessions[j].ID
	})
	return sessions, nil
}

func (h *HistoryStore) Transcript(live []state.SessionInfo, id string) (TranscriptResponse, error) {
	return h.transcript(context.Background(), live, id, 0)
}

// TranscriptWindow returns stable-indexed messages from a complete normalized
// transcript without sending the rest of a potentially very large history to
// an interactive client. End is exclusive; a negative End means no upper
// bound. Role optionally selects user, assistant, or searchable tool events.
func (h *HistoryStore) TranscriptWindow(live []state.SessionInfo, id string, options TranscriptWindowOptions) (TranscriptResponse, error) {
	if options.End < 0 || options.End-options.Start > MaxTranscriptWindowSpan {
		options.End = options.Start + MaxTranscriptWindowSpan
	}
	source, ok := h.find(live, id)
	if !ok {
		return TranscriptResponse{}, ErrHistoryNotFound
	}
	if source.archived != nil {
		return h.archivedTranscriptWindow(*source.archived, options)
	}
	session, path, tool, err := h.describeSource(source, countNone)
	if err != nil {
		return TranscriptResponse{}, err
	}
	if path == "" || !session.ConversationAvailable {
		return TranscriptResponse{}, ErrHistoryNotFound
	}
	messages, messageCount, skipped, matchedExpected, err := normalizeTranscriptWindow(path, tool, options)
	if err != nil {
		return TranscriptResponse{}, fmt.Errorf("read history transcript window %s: %w", id, err)
	}
	if options.ExpectedMessage != "" && !matchedExpected {
		return TranscriptResponse{}, ErrHistoryChanged
	}
	session.MessageCount = messageCount
	session.MessageCountUncounted = false
	session.SkippedRecords = skipped
	nextIndex := options.End
	hasMore := nextIndex >= 0 && nextIndex < messageCount
	return TranscriptResponse{
		SchemaVersion:  SchemaVersion,
		Session:        session,
		Messages:       messages,
		Truncated:      len(messages) != messageCount,
		HasMore:        hasMore,
		NextIndex:      nextIndex,
		SkippedRecords: skipped,
	}, nil
}

// TranscriptLimited reads at most maxBytes from the normalized conversation
// file. A non-positive limit preserves the unbounded recall behavior.
func (h *HistoryStore) TranscriptLimited(live []state.SessionInfo, id string, maxBytes int64) (TranscriptResponse, error) {
	return h.transcript(context.Background(), live, id, maxBytes)
}

func (h *HistoryStore) TranscriptLimitedContext(ctx context.Context, live []state.SessionInfo, id string, maxBytes int64) (TranscriptResponse, error) {
	return h.transcript(ctx, live, id, maxBytes)
}

// TranscriptPreview returns a tail-bounded window suitable for interactive
// rendering. The full Transcript and Raw contracts remain available to
// integrations that deliberately request the complete history.
func (h *HistoryStore) TranscriptPreview(live []state.SessionInfo, id string, maxBytes int64, maxMessages int) (TranscriptResponse, error) {
	source, ok := h.find(live, id)
	if !ok {
		return TranscriptResponse{}, ErrHistoryNotFound
	}
	if source.archived != nil {
		response := h.archivedTranscript(*source.archived)
		if len(response.Messages) > maxMessages {
			response.Messages = response.Messages[len(response.Messages)-maxMessages:]
			response.Truncated = true
		}
		return response, nil
	}
	session, path, tool, err := h.describeSource(source, countNone)
	if err != nil {
		return TranscriptResponse{}, err
	}
	if path == "" || !session.ConversationAvailable {
		return TranscriptResponse{}, ErrHistoryNotFound
	}
	messages, skipped, truncated, err := normalizeTranscriptTail(path, tool, maxBytes, maxMessages)
	if err != nil {
		return TranscriptResponse{}, fmt.Errorf("read history transcript preview %s: %w", id, err)
	}
	session.MessageCount = len(messages)
	session.MessageCountUncounted = false
	session.SkippedRecords = skipped
	return TranscriptResponse{
		SchemaVersion:  SchemaVersion,
		Session:        session,
		Messages:       messages,
		Truncated:      truncated,
		SkippedRecords: skipped,
	}, nil
}

func (h *HistoryStore) transcript(ctx context.Context, live []state.SessionInfo, id string, maxBytes int64) (TranscriptResponse, error) {
	source, ok := h.find(live, id)
	if !ok {
		return TranscriptResponse{}, ErrHistoryNotFound
	}
	if source.archived != nil {
		if err := ctx.Err(); err != nil {
			return TranscriptResponse{}, err
		}
		return h.archivedTranscript(*source.archived), nil
	}
	session, path, tool, err := h.describeSource(source, countNone)
	if err != nil {
		return TranscriptResponse{}, err
	}
	if path == "" || !session.ConversationAvailable {
		return TranscriptResponse{}, ErrHistoryNotFound
	}
	messages, skipped, err := normalizeTranscriptContext(ctx, path, tool, maxBytes)
	if err != nil {
		return TranscriptResponse{}, fmt.Errorf("read history transcript %s: %w", id, err)
	}
	session.MessageCount = len(messages)
	session.MessageCountUncounted = false
	session.SkippedRecords = skipped
	return TranscriptResponse{
		SchemaVersion:  SchemaVersion,
		Session:        session,
		Messages:       messages,
		SkippedRecords: skipped,
	}, nil
}

func (h *HistoryStore) Raw(live []state.SessionInfo, id string) ([]byte, error) {
	source, ok := h.find(live, id)
	if !ok {
		return nil, ErrHistoryNotFound
	}
	if source.archived != nil {
		return json.Marshal(source.archived.Prompts)
	}
	_, path, _, err := h.describeSource(source, countNone)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, ErrHistoryNotFound
	}
	encoded, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrHistoryNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read raw history %s: %w", id, err)
	}
	return encoded, nil
}

type resolvedHistorySource struct {
	managed  *backup.Session
	external *watch.ResumableSession
	archived *watch.ArchivedClaudeConversation
}

func (h *HistoryStore) find(live []state.SessionInfo, id string) (resolvedHistorySource, bool) {
	for _, session := range backup.CollectSessions(live, h.options.RunnerStateDir) {
		if session.ID == id {
			copy := session
			return resolvedHistorySource{managed: &copy}, true
		}
	}
	if h.options.DiscoverProviderHistory {
		for _, session := range h.providerConversations() {
			if externalHistoryID(session.Tool, session.SessionID) == id {
				copy := session
				return resolvedHistorySource{external: &copy}, true
			}
		}
		for _, session := range h.archivedClaudeConversations() {
			if archivedHistoryID(session.SessionID) == id {
				copy := session
				return resolvedHistorySource{archived: &copy}, true
			}
		}
	}
	return resolvedHistorySource{}, false
}

// markUnreadable degrades one history row instead of the whole listing. The
// session keeps its identity, name, cwd, and timestamps, so it stays findable
// and resumable; only its conversation is reported as unavailable, with the
// reason and the next action attached to the row itself.
func markUnreadable(session HistorySession, fallbackID string, err error) HistorySession {
	if session.ID == "" {
		session.ID = fallbackID
	}
	if session.Name == "" {
		session.Name = session.ID
	}
	session.Unreadable = true
	session.ConversationAvailable = false
	session.MessageCount = 0
	session.MessageCountUncounted = false
	session.UnreadableReason = fmt.Sprintf(
		"transcript could not be read (%v); the session record is intact — check that its provider file and any network volume are reachable, then reload history",
		err)
	return session
}

func (h *HistoryStore) describeSource(source resolvedHistorySource, counting messageCountMode) (HistorySession, string, string, error) {
	if source.managed != nil {
		return h.describe(*source.managed, counting)
	}
	if source.external != nil {
		return h.describeExternal(*source.external, counting)
	}
	return HistorySession{}, "", "", ErrHistoryNotFound
}

func (h *HistoryStore) describeArchived(source watch.ArchivedClaudeConversation) HistorySession {
	name := compactHistoryTitle(source.FirstUserMessage)
	if name == "" {
		name = source.SessionID
	}
	return HistorySession{
		ID: archivedHistoryID(source.SessionID), Name: name, Tool: "claude",
		ProviderSessionID: source.SessionID, CWD: source.Cwd, Machine: h.options.Machine,
		CreatedAt: source.ModifiedAt, LastActivityAt: source.ModifiedAt,
		ConversationUpdatedAt: source.ModifiedAt,
		MessageCount:          len(source.Prompts), ConversationAvailable: len(source.Prompts) > 0,
		External: true, PromptHistoryOnly: true,
		SourceFingerprint: archivedSourceFingerprint(source),
	}
}

func (h *HistoryStore) describe(source backup.Session, counting messageCountMode) (HistorySession, string, string, error) {
	// Backup opt-out controls external upload only. These local, authenticated
	// recall endpoints remain able to read the user's own conversation.
	source.OptOut = false
	path, conversationTool := (backup.Resolver{
		ClaudeProjectsDir: h.options.ClaudeProjectsDir,
		CodexSessionsDir:  h.options.CodexSessionsDir,
		RunnerStateDir:    h.options.RunnerStateDir,
		Now:               h.options.Now,
	}).Resolve(source)
	tool := historyTool(source.Tool, conversationTool)
	result := HistorySession{
		ID: source.ID, Name: historyDisplayName(source), Tool: tool,
		ProviderSessionID: providerSessionID(source, tool), CWD: source.CWD,
		Machine: h.options.Machine, CreatedAt: source.CreatedAt,
		LastActivityAt: source.LastActivityAt, CreatorKind: source.CreatorKind,
		CreatorID: source.CreatorID, ReopenedAs: source.ReopenedAs,
		ResumedFrom: source.ResumedFrom, MovedToEndpoint: source.MovedToEndpoint,
		MovedToSessionID: source.MovedToSessionID, MovedFromEndpoint: source.MovedFromEndpoint,
		MovedFromSessionID: source.MovedFromSessionID,
	}
	if path == "" {
		return result, "", tool, nil
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return result, "", tool, nil
	}
	if err != nil {
		// The partial session travels with the error so a listing can degrade
		// this row (see markUnreadable) while a single-session fetch still
		// fails loudly with the same instructional message.
		return result, "", tool, fmt.Errorf("stat history transcript %s: %w", source.ID, err)
	}
	if !info.Mode().IsRegular() {
		return result, "", tool, nil
	}
	// Older Codex runners sometimes exited after the provider created its
	// rollout but before the app-server thread id reached Sessions metadata.
	// The provider's immutable session_meta remains the authoritative recovery
	// handle, so recover it directly from the already-resolved source.
	if tool == "codex" && result.ProviderSessionID == "" {
		if providerID, _, identityErr := watch.ReadCodexConversationIdentity(path); identityErr == nil {
			result.ProviderSessionID = providerID
		}
	}
	if tool == "claude" && result.ProviderSessionID == "" {
		providerID := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		if isProviderUUID(providerID) {
			result.ProviderSessionID = providerID
		}
	}
	result.ConversationAvailable = true
	updated, fromRecord := conversationUpdatedAt(path, info)
	result.ConversationUpdatedAt = updated
	result.ConversationUpdatedApproximate = !fromRecord
	result.LastActivityAt = max(result.LastActivityAt, updated)
	result.Surface = conversationSurface(path, tool)
	result.SourceFingerprint = historySourceFingerprint(path, info)
	count, skipped, counted, err := h.countMessages(counting, path, tool, info)
	if err != nil {
		return result, "", tool, fmt.Errorf("count history transcript %s: %w", source.ID, err)
	}
	result.MessageCount = count
	result.SkippedRecords = skipped
	result.MessageCountUncounted = !counted
	return result, path, tool, nil
}

func (h *HistoryStore) describeExternal(source watch.ResumableSession, counting messageCountMode) (HistorySession, string, string, error) {
	tool := strings.TrimSpace(source.Tool)
	name := compactHistoryTitle(source.Title)
	if name == "" {
		name = compactHistoryTitle(source.FirstUserMessage)
	}
	if name == "" {
		name = source.SessionID
	}
	result := HistorySession{
		ID: externalHistoryID(tool, source.SessionID), Name: name, Tool: tool,
		ProviderSessionID: source.SessionID, CWD: source.Cwd, Machine: h.options.Machine,
		CreatedAt: int64(source.ModifiedAt), LastActivityAt: int64(source.ModifiedAt),
		ConversationAvailable: source.SourcePath != "", External: true,
	}
	if source.SourcePath == "" {
		return result, "", tool, nil
	}
	info, err := os.Stat(source.SourcePath)
	if errors.Is(err, os.ErrNotExist) {
		return result, "", tool, nil
	}
	if err != nil {
		return result, "", tool, fmt.Errorf("stat provider history %s: %w", source.SessionID, err)
	}
	if !info.Mode().IsRegular() {
		return result, "", tool, nil
	}
	result.SourceFingerprint = historySourceFingerprint(source.SourcePath, info)
	updated, fromRecord := conversationUpdatedAt(source.SourcePath, info)
	result.ConversationUpdatedAt = updated
	result.ConversationUpdatedApproximate = !fromRecord
	// A provider conversation has no Sessions record behind it, so mtime was the
	// only activity time it had. Where the conversation stamped its own last
	// record, that replaces mtime rather than being maxed with it: taking the
	// larger of the two would keep exactly the inflated value a copy produces.
	if fromRecord {
		result.LastActivityAt = updated
	}
	// The scan already parsed the provider metadata that carries provenance, so
	// take it from there rather than opening the file a second time.
	result.Surface = source.Surface
	count, skipped, counted, err := h.countMessages(counting, source.SourcePath, tool, info)
	if err != nil {
		return result, "", tool, fmt.Errorf("count provider history %s: %w", source.SessionID, err)
	}
	result.MessageCount = count
	result.SkippedRecords = skipped
	result.MessageCountUncounted = !counted
	return result, source.SourcePath, tool, nil
}

// conversationUpdatedAt answers when the conversation was last written to, and
// says which source the answer came from.
//
// The provider's own records are preferred over the file's modification time
// because mtime is metadata about the file and does not survive being copied.
// A `cp -R` of this machine's history was enough to make all 203 conversations
// report the same instant, which erased the browser's primary ordering; the
// records themselves carry RFC3339 timestamps that travel with the bytes. The
// read is a single bounded seek to the tail, so it costs the same on the 1.1 GB
// transcript here as on a small one.
//
// mtime remains the fallback for a transcript that stamped nothing -- a
// single-record bridge file, say -- and the caller is told which it got so a
// copy artefact is never presented as recency.
func conversationUpdatedAt(path string, info os.FileInfo) (int64, bool) {
	if recorded, ok := watch.ConversationRecordedActivity(path); ok {
		return recorded.UnixMilli(), true
	}
	return info.ModTime().UnixMilli(), false
}

// conversationSurface reads where a managed conversation was started from. Both
// readers are the bounded ones the resolvers already use -- Codex's session_meta
// first line, Claude's transcript prefix -- so this adds a bounded read per row
// and no new parser.
func conversationSurface(path, tool string) *watch.ConversationSurface {
	var (
		surface watch.ConversationSurface
		ok      bool
	)
	switch tool {
	case "codex":
		surface, ok = watch.ReadCodexConversationSurface(path)
	case "claude":
		surface, ok = watch.ReadClaudeConversationSurface(path)
	default:
		return nil
	}
	if !ok {
		return nil
	}
	return &surface
}
