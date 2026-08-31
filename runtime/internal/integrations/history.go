// Package integrations owns the stable, versioned contracts consumed by
// external Sessions integrations. It does not call any external service.
package integrations

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ansi"
	"github.com/somewhere-tech/sessions/runtime/internal/backup"
	"github.com/somewhere-tech/sessions/runtime/internal/providerargs"
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

func (h *HistoryStore) providerConversations() []watch.ResumableSession {
	h.providerMu.Lock()
	defer h.providerMu.Unlock()
	now := time.Now()
	if !h.providerCachedAt.IsZero() && now.Sub(h.providerCachedAt) < 2*time.Second {
		return append([]watch.ResumableSession(nil), h.providerCache...)
	}
	h.providerCache = watch.ScanResumableConversationsIn(h.options.ClaudeProjectsDir, h.options.CodexSessionsDir)
	h.providerCachedAt = now
	return append([]watch.ResumableSession(nil), h.providerCache...)
}

// ResumableProviderConversations returns the same short-lived provider scan
// used by History. Resume surfaces call this after History so one request does
// not recursively scan every Claude and Codex file twice.
func (h *HistoryStore) ResumableProviderConversations() []watch.ResumableSession {
	return h.providerConversations()
}

func (h *HistoryStore) archivedClaudeConversations() []watch.ArchivedClaudeConversation {
	h.providerMu.Lock()
	defer h.providerMu.Unlock()
	now := time.Now()
	if !h.archiveCachedAt.IsZero() && now.Sub(h.archiveCachedAt) < 2*time.Second {
		return append([]watch.ArchivedClaudeConversation(nil), h.archiveCache...)
	}
	h.archiveCache = watch.ScanArchivedClaudeConversations(h.options.ClaudeHistoryPath)
	resumable := make(map[string]bool, len(h.providerCache))
	for _, source := range h.providerCache {
		if source.Tool == "claude" {
			resumable[source.SessionID] = true
		}
	}
	filtered := h.archiveCache[:0]
	for _, source := range h.archiveCache {
		if !resumable[source.SessionID] {
			filtered = append(filtered, source)
		}
	}
	h.archiveCache = filtered
	h.archiveCachedAt = now
	return append([]watch.ArchivedClaudeConversation(nil), h.archiveCache...)
}

func historyProviderKey(tool, providerID string) string {
	return strings.TrimSpace(tool) + ":" + strings.TrimSpace(providerID)
}

func externalHistoryID(tool, providerID string) string {
	return "provider:" + historyProviderKey(tool, providerID)
}

func archivedHistoryID(providerID string) string {
	return "provider-history:claude:" + strings.TrimSpace(providerID)
}

func archivedSourceFingerprint(source watch.ArchivedClaudeConversation) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%s\x00%d\x00%d", source.SessionID, source.ModifiedAt, len(source.Prompts))
	for _, prompt := range source.Prompts {
		_, _ = fmt.Fprintf(hash, "\x00%d\x00%s", prompt.Timestamp, prompt.Text)
	}
	return fmt.Sprintf("%x", hash.Sum(nil)[:16])
}

func (h *HistoryStore) archivedTranscript(source watch.ArchivedClaudeConversation) TranscriptResponse {
	session := h.describeArchived(source)
	messages := make([]TranscriptMessage, 0, len(source.Prompts))
	for index, prompt := range source.Prompts {
		timestamp := time.UnixMilli(prompt.Timestamp).UTC().Format(time.RFC3339Nano)
		messages = append(messages, TranscriptMessage{
			Index: index, ID: fmt.Sprintf("prompt-history:%s:%d", source.SessionID, index),
			Role: "user", Text: prompt.Text, Timestamp: &timestamp,
		})
	}
	return TranscriptResponse{SchemaVersion: SchemaVersion, Session: session, Messages: messages}
}

func (h *HistoryStore) archivedTranscriptWindow(source watch.ArchivedClaudeConversation, options TranscriptWindowOptions) (TranscriptResponse, error) {
	response := h.archivedTranscript(source)
	response.Session.MessageCount = len(response.Messages)
	response.Session.MessageCountUncounted = false
	end := options.End
	if end < 0 || end > len(response.Messages) {
		end = len(response.Messages)
	}
	start := min(max(0, options.Start), end)
	response.Messages = response.Messages[start:end]
	response.Truncated = len(response.Messages) != response.Session.MessageCount
	response.NextIndex = end
	response.HasMore = end < response.Session.MessageCount
	if options.ExpectedMessage != "" {
		matched := options.ExpectedIndex >= 0 && options.ExpectedIndex < len(source.Prompts) &&
			fmt.Sprintf("prompt-history:%s:%d", source.SessionID, options.ExpectedIndex) == options.ExpectedMessage
		if !matched {
			return TranscriptResponse{}, ErrHistoryChanged
		}
	}
	return response, nil
}

// historyDisplayName names a conversation in history. A Sessions name that is
// not a launch-time auto-name outranks a title Claude generated, but never a
// title set inside Claude itself.
//
// The provider's own title and its shaping come from
// state.ProviderConversationTitle, which is the same derivation the daemon
// adopts into the live session's name, so a conversation cannot be called one
// thing on the session card and another in history.
func historyDisplayName(source backup.Session) string {
	if !genericHistoryName(source.Name) {
		if custom := compactHistoryTitle(source.ClaudeCustomTitle); custom != "" {
			return custom
		}
		if name := compactHistoryTitle(source.Name); name != "" {
			return name
		}
	}
	if title := state.ProviderConversationTitle(source.ClaudeCustomTitle, source.ClaudeAITitle); title != "" {
		return title
	}
	for _, candidate := range []string{source.Description, source.Name} {
		if value := compactHistoryTitle(candidate); value != "" {
			return value
		}
	}
	return source.ID
}

func genericHistoryName(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, prefix := range []string{"claude - ", "claude — ", "codex - ", "codex — ", "shell - ", "shell — "} {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

// compactHistoryTitle is the shared conversation-title shaping. It lives in
// state because the daemon applies the same shaping when it adopts a provider
// title as a session name.
func compactHistoryTitle(value string) string {
	return state.CompactConversationTitle(value)
}

func providerSessionID(source backup.Session, tool string) string {
	switch tool {
	case "claude":
		if value := strings.TrimSpace(source.ClaudeSessionID); value != "" {
			return value
		}
		return extractHistoryClaudeSessionID(source.Args)
	case "codex":
		if value := strings.TrimSpace(source.ConversationID); value != "" {
			return value
		}
		return extractCodexConversationID(source.Args)
	default:
		return ""
	}
}

// These two used to be narrower than every other reader of the same argv: the
// Claude one missed `-r <uuid>` and the joined `--session-id=<uuid>` form, and
// the Codex one saw only the bare `resume` subcommand, so a conversation
// reopened with `codex --resume <uuid>` had no identity in history.
func extractHistoryClaudeSessionID(args []string) string {
	return providerargs.ClaudeSessionID(args)
}

func extractCodexConversationID(args []string) string {
	return providerargs.CodexConversationID(args)
}

func isProviderUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}

func historySourceFingerprint(path string, info os.FileInfo) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d", path, info.Size(), info.ModTime().UnixNano())))
	return fmt.Sprintf("%x", sum[:16])
}

// messageCount reports the normalized message count of one transcript and how
// many torn records were skipped to get it. Counting still parses the file —
// a message count is not derivable from line counts — but it never retains the
// messages, so listing a multi-gigabyte history costs constant memory instead
// of a full transcript per session.
func (h *HistoryStore) messageCount(path, tool string, info os.FileInfo) (int, int, error) {
	h.cacheMu.Lock()
	cached, ok := h.cache[path]
	if ok && cached.size == info.Size() && cached.modTimeNano == info.ModTime().UnixNano() {
		h.cacheClock++
		cached.used = h.cacheClock
		h.cache[path] = cached
		h.cacheMu.Unlock()
		return cached.count, cached.skipped, nil
	}
	h.cacheMu.Unlock()
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	// discardEveryMessage keeps the streaming normalizer's counting and
	// skip-accounting while retaining none of the messages it builds.
	discardEveryMessage := func(TranscriptMessage) bool { return false }
	_, count, skipped, err := normalizeTranscriptReaderSelected(
		context.Background(), bufio.NewReader(file), path, tool, discardEveryMessage)
	closeErr := file.Close()
	if err != nil {
		return 0, 0, err
	}
	if closeErr != nil {
		return 0, 0, closeErr
	}
	h.cacheMu.Lock()
	h.cacheClock++
	h.cache[path] = historyCacheEntry{
		size: info.Size(), modTimeNano: info.ModTime().UnixNano(),
		count: count, skipped: skipped, used: h.cacheClock,
	}
	h.evictHistoryCacheLocked()
	h.cacheMu.Unlock()
	return count, skipped, nil
}

// evictHistoryCacheLocked drops the least recently used quarter once the cache
// passes its bound. Transcript paths are unbounded over a daemon's lifetime, so
// the cache must have a ceiling; dropping a quarter keeps the O(n) sweep rare.
func (h *HistoryStore) evictHistoryCacheLocked() {
	if len(h.cache) <= maxHistoryCacheEntries {
		return
	}
	cutoff := h.cacheClock - uint64(maxHistoryCacheEntries*3/4)
	for path, entry := range h.cache {
		if entry.used <= cutoff {
			delete(h.cache, path)
		}
	}
}

func historyTool(tool state.SessionTool, resolved string) string {
	if resolved != "" {
		return resolved
	}
	switch tool {
	case state.ToolClaude:
		return "claude"
	case state.ToolCodex:
		return "codex"
	case state.ToolTerminal:
		return "terminal"
	default:
		return string(tool)
	}
}

func normalizeTranscript(path, tool string, maxBytes int64) ([]TranscriptMessage, int, error) {
	return normalizeTranscriptContext(context.Background(), path, tool, maxBytes)
}

func normalizeTranscriptContext(ctx context.Context, path, tool string, maxBytes int64) ([]TranscriptMessage, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()

	var source io.Reader = file
	if maxBytes > 0 {
		source = io.LimitReader(file, maxBytes)
	}
	return normalizeTranscriptReaderContext(ctx, bufio.NewReader(source), path, tool)
}

func normalizeTranscriptWindow(path, tool string, options TranscriptWindowOptions) ([]TranscriptMessage, int, int, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, false, err
	}
	defer file.Close()

	matchedExpected := options.ExpectedMessage == ""
	messages, messageCount, skipped, err := normalizeTranscriptReaderSelected(context.Background(), bufio.NewReader(file), path, tool, func(message TranscriptMessage) bool {
		if message.Index == options.ExpectedIndex && message.ID == options.ExpectedMessage {
			matchedExpected = true
		}
		if message.Index < options.Start || (options.End >= 0 && message.Index >= options.End) {
			return false
		}
		return options.Role == "" || message.Role == options.Role
	})
	return messages, messageCount, skipped, matchedExpected, err
}

func normalizeTranscriptTail(path, tool string, maxBytes int64, maxMessages int) ([]TranscriptMessage, int, bool, error) {
	if maxBytes <= 0 || maxMessages <= 0 {
		return nil, 0, false, errors.New("preview limits must be positive: pass a positive byte and message budget")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, 0, false, err
	}
	offset := max(int64(0), info.Size()-maxBytes)
	truncated := offset > 0
	var reader *bufio.Reader
	if offset > 0 {
		// Keep a record that begins exactly at the window boundary. Otherwise
		// discard the partial first JSONL record before normalization.
		if _, err := file.Seek(offset-1, io.SeekStart); err != nil {
			return nil, 0, false, err
		}
		previous := []byte{0}
		if _, err := io.ReadFull(file, previous); err != nil {
			return nil, 0, false, err
		}
		reader = bufio.NewReader(io.LimitReader(file, maxBytes))
		if previous[0] != '\n' {
			if _, err := reader.ReadBytes('\n'); err != nil && !errors.Is(err, io.EOF) {
				return nil, 0, false, err
			}
		} else if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return nil, 0, false, err
		} else {
			reader = bufio.NewReader(io.LimitReader(file, maxBytes))
		}
	} else {
		reader = bufio.NewReader(io.LimitReader(file, maxBytes))
	}
	messages, skipped, err := normalizeTranscriptReader(reader, path, tool)
	if err != nil {
		return nil, 0, false, err
	}
	if len(messages) > maxMessages {
		messages = messages[len(messages)-maxMessages:]
		truncated = true
	}
	return messages, skipped, truncated, nil
}

func normalizeTranscriptReader(reader *bufio.Reader, path, tool string) ([]TranscriptMessage, int, error) {
	return normalizeTranscriptReaderContext(context.Background(), reader, path, tool)
}

func normalizeTranscriptReaderContext(ctx context.Context, reader *bufio.Reader, path, tool string) ([]TranscriptMessage, int, error) {
	messages, _, skipped, err := normalizeTranscriptReaderSelected(ctx, reader, path, tool, nil)
	return messages, skipped, err
}

// normalizeTranscriptReaderSelected streams one provider transcript. It follows
// the torn-record policy stated in runtime/internal/integrations/errors.go: a
// JSONL line it cannot decode is skipped, counted, and reported as the third
// result, so a truncated tail from a power cut costs exactly that record and
// the caller can still tell the read was degraded. include may be nil to keep
// every message, or return false for every message to count without retaining.
func normalizeTranscriptReaderSelected(
	ctx context.Context,
	reader *bufio.Reader,
	path, tool string,
	include func(TranscriptMessage) bool,
) ([]TranscriptMessage, int, int, error) {
	messages := make([]TranscriptMessage, 0)
	relayCalls := make(map[string]string)
	lineIndex := 0
	messageIndex := 0
	skipped := 0
	for {
		if lineIndex%256 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, 0, 0, err
			}
		}
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := strings.TrimSpace(string(line))
			currentIndex := lineIndex
			lineIndex++
			if trimmed != "" {
				var decoded map[string]any
				if json.Unmarshal([]byte(trimmed), &decoded) != nil {
					skipped++
				} else {
					if tool == "codex" {
						normalized := watch.NormalizeCodexRolloutLine(decoded, watch.CodexNormalizeContext{
							RolloutBasename: filepath.Base(path), LineIndex: currentIndex,
						})
						for _, event := range normalized.Events {
							for _, message := range transcriptMessages(event, relayCalls) {
								message.Index = messageIndex
								message.ID = transcriptMessageID(message)
								messageIndex++
								if include == nil || include(message) {
									messages = append(messages, message)
								}
							}
						}
					} else {
						for _, message := range transcriptMessages(decoded, relayCalls) {
							message.Index = messageIndex
							message.ID = transcriptMessageID(message)
							messageIndex++
							if include == nil || include(message) {
								messages = append(messages, message)
							}
						}
					}
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			// The file itself stopped being readable mid-stream. Unlike a torn
			// record this is not recoverable by skipping ahead, so the caller
			// is told rather than handed a silently short conversation; a
			// listing degrades that one row instead of dropping every session.
			return nil, 0, skipped, fmt.Errorf("read transcript %s: %w", path, readErr)
		}
	}
	return messages, messageIndex, skipped, nil
}

func transcriptMessageID(message TranscriptMessage) string {
	timestamp := ""
	if message.Timestamp != nil {
		timestamp = *message.Timestamp
	}
	sum := sha256.Sum256([]byte(message.Role + "\x00" + message.Kind + "\x00" + timestamp + "\x00" + message.Text))
	return fmt.Sprintf("%x", sum[:16])
}

func transcriptMessages(event map[string]any, relayCalls map[string]string) []TranscriptMessage {
	message, ok := event["message"].(map[string]any)
	if !ok {
		return nil
	}
	role, _ := message["role"].(string)
	if role != "user" && role != "assistant" {
		return nil
	}
	timestamp := normalizedTimestamp(event["timestamp"])
	result := make([]TranscriptMessage, 0, 2)
	if text := contentText(message["content"]); text != "" {
		text = stripTranscriptANSI(text)
		if !isHiddenProviderControlMessage(text) {
			messageRole := role
			if role == "user" && isSyntheticUserMessage(text) {
				messageRole = "tool"
			}
			kind := ""
			if messageRole == "tool" {
				kind = "automation"
			}
			result = append(result, TranscriptMessage{Role: messageRole, Kind: kind, Text: text, Timestamp: timestamp})
		}
	}
	blocks, _ := message["content"].([]any)
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "tool_use":
			name, _ := block["name"].(string)
			if !isRelayTool(name) {
				continue
			}
			id, _ := block["id"].(string)
			if id != "" {
				relayCalls[id] = name
			}
			if text := relayToolRequest(name, block["input"]); text != "" {
				result = append(result, TranscriptMessage{Role: "tool", Kind: relayToolKind(name), Text: text, Timestamp: timestamp})
			}
		case "tool_result":
			id, _ := block["tool_use_id"].(string)
			name := relayCalls[id]
			if name == "" {
				continue
			}
			if text := relayToolResult(name, block["content"]); text != "" {
				result = append(result, TranscriptMessage{Role: "tool", Kind: relayToolKind(name), Text: text, Timestamp: timestamp})
			}
		}
	}
	return result
}

func stripTranscriptANSI(text string) string {
	return strings.TrimSpace(ansi.Strip(text))
}

func isHiddenProviderControlMessage(text string) bool {
	trimmed := strings.TrimSpace(text)
	for _, prefix := range []string{
		"<command-name>", "<command-message>", "<command-args>", "<command-stdout>",
		"<local-command-", "<system-reminder>", "<recommended_plugins>", "<environment_context>",
		"# AGENTS.md instructions", "# CLAUDE.md instructions",
		"This session is being continued from a previous conversation",
		"Caveat: The messages below were generated by the user while",
	} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

func isSyntheticUserMessage(text string) bool {
	trimmed := strings.TrimSpace(text)
	for _, prefix := range []string{
		"<task-notification>", "<system-reminder>", "<local-command-", "<command-message>",
	} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	if strings.HasPrefix(trimmed, "[") {
		line := trimmed
		if end := strings.IndexByte(line, ']'); end >= 0 {
			line = strings.ToUpper(line[:end+1])
			return strings.Contains(line, " TICK") ||
				strings.Contains(line, " AUTOMATION") ||
				strings.Contains(line, " ROUTINE")
		}
	}
	return false
}

func contentText(content any) string {
	switch value := content.(type) {
	case string:
		return strings.TrimSpace(value)
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			block, ok := item.(map[string]any)
			if !ok || block["type"] != "text" {
				continue
			}
			if text, ok := block["text"].(string); ok && strings.TrimSpace(text) != "" {
				parts = append(parts, strings.TrimSpace(text))
			}
		}
		return strings.Join(parts, "\n\n")
	default:
		return ""
	}
}

var searchableRelayTools = map[string]struct{}{
	"agent":                  {},
	"spawn_agent":            {},
	"send_message":           {},
	"send_message_to_thread": {},
	"followup_task":          {},
	"create_thread":          {},
	"fork_thread":            {},
	"read_thread":            {},
	"wait_agent":             {},
	"wait_threads":           {},
	"handoff_thread":         {},
}

func isRelayTool(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for candidate := range searchableRelayTools {
		if name == candidate || strings.HasSuffix(name, "__"+candidate) || strings.HasSuffix(name, "."+candidate) {
			return true
		}
	}
	return false
}

func relayToolRequest(name string, input any) string {
	values, _ := input.(map[string]any)
	if len(values) == 0 {
		return ""
	}
	target := firstString(values, "target", "thread_id", "threadId", "recipient", "task_name", "subagent_type")
	body := firstString(values, "message", "prompt", "task", "objective", "description")
	if body == "" {
		return ""
	}
	label := relayToolLabel(name)
	if target != "" {
		label += " to " + target
	}
	return boundedRelayText(label+": "+body, 64<<10)
}

func relayToolKind(name string) string {
	switch relayToolLabel(name) {
	case "agent", "spawn_agent", "create_thread", "fork_thread":
		return "delegation"
	case "send_message", "send_message_to_thread", "followup_task", "handoff_thread":
		return "handoff"
	default:
		return "status"
	}
}

func relayToolResult(name string, content any) string {
	text := contentText(content)
	if text == "" {
		if value, ok := content.(string); ok {
			text = strings.TrimSpace(value)
		}
	}
	if text == "" {
		return ""
	}
	return boundedRelayText(relayToolLabel(name)+" result: "+text, 64<<10)
}

func relayToolLabel(name string) string {
	normalized := strings.ToLower(strings.TrimSpace(name))
	for candidate := range searchableRelayTools {
		if normalized == candidate || strings.HasSuffix(normalized, "__"+candidate) || strings.HasSuffix(normalized, "."+candidate) {
			return candidate
		}
	}
	return normalized
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func boundedRelayText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

func normalizedTimestamp(value any) *string {
	switch timestamp := value.(type) {
	case string:
		if timestamp == "" {
			return nil
		}
		return &timestamp
	case float64:
		var parsed time.Time
		if timestamp > -100_000_000_000 && timestamp < 100_000_000_000 {
			seconds := int64(timestamp)
			nanos := int64((timestamp - float64(seconds)) * float64(time.Second))
			parsed = time.Unix(seconds, nanos)
		} else {
			parsed = time.UnixMilli(int64(timestamp))
		}
		formatted := parsed.UTC().Format(time.RFC3339Nano)
		return &formatted
	default:
		return nil
	}
}
