package state

import "encoding/json"

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneUint64Pointer(value *uint64) *uint64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneFloat64Pointer(value *float64) *float64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneIntPointer(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

type SessionTool string

const (
	ToolClaude              SessionTool = "claude-code"
	ToolCodex               SessionTool = "codex"
	ToolTerminal            SessionTool = "terminal"
	ToolLane                SessionTool = "lane"
	KindLane                            = "lane"
	KindCodexAppServer                  = "codex-app-server"
	KindClaudeStructured                = "claude-structured"
	DescriptionExplicit                 = "explicit"
	DescriptionFirstMessage             = "first-message"
)

// Where a session's stored name came from, and therefore who may change it.
//
// A session card used to keep whatever name it was launched with while Claude
// titled the same conversation something else everywhere the provider itself
// showed it, so searching Sessions for the name the user could see in Claude
// found nothing. The name now follows the provider's own conversation title
// unless a person has said otherwise.
//
// NameSourceLaunch is also the value an older session with no recorded source
// takes, which makes the change migration-free: an untouched session simply
// becomes adoptable. That default cannot distinguish a session whose name was
// set by an earlier `sessions rename` from one that still carries its launch
// name, so such a session can be adopted once by the provider title. Renaming
// it again pins it permanently, and that is the whole cost of not writing a
// migration for a field that did not exist.
const (
	NameSourceLaunch   = "launch"
	NameSourceProvider = "provider"
	NameSourceExplicit = "explicit"
)

type SessionInfo struct {
	ID                     string            `json:"id"`
	Name                   string            `json:"name,omitempty"`
	NameSource             string            `json:"name_source,omitempty"`
	Description            string            `json:"description"`
	DescriptionSource      string            `json:"description_source,omitempty"`
	Tags                   map[string]string `json:"tags,omitempty"`
	Kind                   string            `json:"kind,omitempty"`
	SpecPath               string            `json:"specPath,omitempty"`
	Cmd                    string            `json:"cmd"`
	Args                   []string          `json:"args"`
	Cwd                    string            `json:"cwd"`
	Profile                string            `json:"profile,omitempty"`
	ConfigDir              string            `json:"config_dir,omitempty"`
	WorktreePath           string            `json:"worktree_path,omitempty"`
	Branch                 string            `json:"branch,omitempty"`
	Base                   string            `json:"base,omitempty"`
	SourceRepo             string            `json:"source_repo,omitempty"`
	Cols                   int               `json:"cols"`
	Rows                   int               `json:"rows"`
	CreatedAt              int64             `json:"createdAt"`
	PID                    int               `json:"pid"`
	RunnerProtocol         int               `json:"runnerProtocol"`
	RunnerVersion          string            `json:"runnerVersion,omitempty"`
	Tool                   SessionTool       `json:"tool"`
	Working                bool              `json:"working"`
	LastDataAt             int64             `json:"lastDataAt"`
	// LastUserMessageAt is transcript-derived and says nothing about who wrote
	// the message. It moves whenever a user-role record appears in the
	// provider's own conversation file, and a provider writes those records for
	// its own internal injections too: a scheduled prompt, a cron tick, a
	// reminder it decided to inject. None of those cross Sessions' input path,
	// and in the transcript they are indistinguishable from a person typing.
	//
	// So this answers "when did this conversation last take a user turn", which
	// is not the question anything asking "when did the user last touch this"
	// wants. For that read LastHumanMessageAt, and do not treat this field as a
	// proxy for human contact.
	LastUserMessageAt *int64 `json:"lastUserMessageAt"`
	// LastHumanMessageAt and LastAgentMessageAt are daemon-owned, and stamped at
	// Sessions' own input boundary rather than read back out of the provider's
	// transcript. That boundary is what makes them trustworthy: a provider's
	// internal injection is written straight into the transcript and never
	// crosses it, so it can move LastUserMessageAt and neither of these.
	//
	// Everything a person sends does cross it -- the HTTP input and submit
	// routes, the WebSocket mux, an attached terminal, `sessions send` -- and
	// arrives with no source-session attribution. An agent relaying to another
	// session arrives through the same routes carrying one. Unattributed is
	// therefore a person, attributed is another session, and a transcript-only
	// user record is neither.
	LastHumanMessageAt *int64 `json:"lastHumanMessageAt"`
	LastAgentMessageAt *int64 `json:"lastAgentMessageAt"`
	IdleReason             string            `json:"idleReason,omitempty"`
	IdleDetail             string            `json:"idleDetail,omitempty"`
	IdleSince              *int64            `json:"idleSince,omitempty"`
	LastSummary            string            `json:"lastSummary,omitempty"`
	// Exited means Sessions reaped a real status for this session's process:
	// an exit code, a signal, or a user-requested end that completed. It is
	// never set because the daemon lost contact. Losing a socket says nothing
	// about whether the process on the other end is still working.
	Exited     bool    `json:"exited"`
	ExitCode   *int    `json:"exitCode"`
	ExitSignal *string `json:"exitSignal"`
	ExitReason string  `json:"exitReason,omitempty"`
	ExitedAt   *int64  `json:"exitedAt"`
	// Unreachable means the daemon cannot currently talk to this session's
	// runner: the socket read failed, the read deadline elapsed, or the daemon
	// restarted out from under a healthy runner. It is a statement about the
	// connection, not about the work. An unreachable session is still a
	// session: it is listed, readable, and attachable, and reconnect or the
	// next discovery pass may reattach it. It is never presented as ended.
	Unreachable       bool   `json:"unreachable,omitempty"`
	UnreachableReason string `json:"unreachableReason,omitempty"`
	UnreachableSince  *int64 `json:"unreachableSince,omitempty"`
	ClaudeCustomTitle      string            `json:"claudeCustomTitle,omitempty"`
	ClaudeAITitle          string            `json:"claudeAiTitle,omitempty"`
	OnIdle                 string            `json:"onIdle,omitempty"`
	Model                  string            `json:"model,omitempty"`
	Effort                 string            `json:"effort,omitempty"`
	Fast                   bool              `json:"fast,omitempty"`
	ConversationID         string            `json:"conversationId,omitempty"`
	RemoteEndpoint         string            `json:"remoteEndpoint,omitempty"`
	ClaudeSessionID        string            `json:"claudeSessionId,omitempty"`
	ContinuedFromHistoryID string            `json:"continuedFromHistoryId,omitempty"`
	ContinuedFromProvider  string            `json:"continuedFromProvider,omitempty"`
	ContinuationMode       string            `json:"continuationMode,omitempty"`
	ImportedMessageCount   int               `json:"importedMessageCount,omitempty"`
	CreatorKind            string            `json:"creator_kind,omitempty"`
	CreatorID              string            `json:"creator_id,omitempty"`
	ParentSessionID        string            `json:"parent_session_id,omitempty"`
	DelegationKind         string            `json:"delegation_kind,omitempty"`
	Permissions            string            `json:"permissions,omitempty"`
	Lifecycle              string            `json:"lifecycle,omitempty"`
	// DisplayParentSessionID is a user-controlled organizational override.
	// nil preserves the creator-ledger hierarchy, a pointer to "" makes the
	// session a visual root, and any other value groups it under that session.
	// It never rewrites trusted creator provenance.
	DisplayParentSessionID *string `json:"display_parent_session_id,omitempty"`
	// SetAsideAt removes a live session from the default working set without
	// changing runner lifecycle, attention, search, or notification truth.
	SetAsideAt *int64 `json:"setAsideAt,omitempty"`
	// Pinned is the user marking a workbench: the session sorts first in every
	// listing and automatic termination keeps its hands off it. It is never
	// omitempty, matching working and exited rather than fast, because an agent
	// choosing what to end has to tell "this session is not pinned" from "this
	// daemon never told me", and an omitted field reads as both.
	Pinned bool `json:"pinned"`
	// MemoryBytes, CPUPercent and ResourceProcesses are what this session costs
	// the machine: resident memory across its whole process tree, percent of
	// one core burned since the previous sample, and how many processes those
	// two numbers cover.
	//
	// All three are pointers, and that is the point of them. A session with no
	// live process, a process the OS refused to describe, and a first sample
	// with nothing to subtract from are all genuinely unknown, and a reader
	// that cannot tell unknown from zero will conclude a machine is idle when
	// it is paging. nil is the only honest encoding of "Sessions does not
	// know"; omitempty keeps it off the wire entirely rather than sending a
	// null that a lenient client would coerce to 0.
	//
	// CPUPercent is a rate measured between two samples, never a lifetime
	// average. 100 is one core saturated; a tree spanning cores reads above
	// 100. See internal/resource.
	MemoryBytes       *uint64  `json:"memoryBytes,omitempty"`
	CPUPercent        *float64 `json:"cpuPercent,omitempty"`
	ResourceProcesses *int     `json:"resourceProcesses,omitempty"`
	// ResourceSampledAt is when the three fields above were measured, in unix
	// milliseconds. It travels with them because a sample is only ever as
	// current as the last tick, and a listing that presents a stale number as
	// live is the same category of lie as presenting unknown as zero.
	ResourceSampledAt  *int64   `json:"resourceSampledAt,omitempty"`
	CreatorAncestry    []string `json:"creator_ancestry,omitempty"`
	RootCreatorKind    string   `json:"root_creator_kind,omitempty"`
	RootCreatorID      string   `json:"root_creator_id,omitempty"`
	ProvenanceStatus   string   `json:"provenance_status,omitempty"`
	ReopenedAs         string   `json:"reopened_as,omitempty"`
	ResumedFrom        string   `json:"resumed_from,omitempty"`
	MovedToEndpoint    string   `json:"moved_to_endpoint,omitempty"`
	MovedToSessionID   string   `json:"moved_to_session_id,omitempty"`
	MovedFromEndpoint  string   `json:"moved_from_endpoint,omitempty"`
	MovedFromSessionID string   `json:"moved_from_session_id,omitempty"`
	EndedByKind        string   `json:"ended_by_kind,omitempty"`
	EndedByID          string   `json:"ended_by_id,omitempty"`
	EndedByName        string   `json:"ended_by_name,omitempty"`
	EndedByClient      string   `json:"ended_by_client,omitempty"`
	EndReason          string   `json:"end_reason,omitempty"`
	EndOperationID     string   `json:"end_operation_id,omitempty"`
}

const (
	IdleReasonNeverStarted = "never-started"
	IdleReasonCompleted    = "completed"
	IdleReasonNeedsInput   = "needs-input"
	IdleReasonFailed       = "failed"
	PermissionsInherit     = "inherit"
	PermissionsConstrained = "constrained"
	PermissionsFull        = "full"
	LifecycleTask          = "task"
	LifecycleSession       = "session"
)

type CreateSessionRequest struct {
	Cmd         string                `json:"cmd,omitempty"`
	Args        []string              `json:"args,omitempty"`
	Cwd         string                `json:"cwd,omitempty"`
	Cols        int                   `json:"cols,omitempty"`
	Rows        int                   `json:"rows,omitempty"`
	Env         map[string]string     `json:"env,omitempty"`
	Name        string                `json:"name,omitempty"`
	Description string                `json:"description,omitempty"`
	Tags        map[string]string     `json:"tags,omitempty"`
	Profile     string                `json:"profile,omitempty"`
	Worktree    bool                  `json:"worktree,omitempty"`
	Base        string                `json:"base,omitempty"`
	Kind        string                `json:"kind,omitempty"`
	SpecPath    string                `json:"specPath,omitempty"`
	OnIdle      string                `json:"onIdle,omitempty"`
	WaitReady   bool                  `json:"waitReady,omitempty"`
	Force       bool                  `json:"force,omitempty"`
	Claude      *ClaudeSessionOptions `json:"claude,omitempty"`
	// CreatorSessionID and CreatorOwnerID are populated from trusted HTTP
	// headers at the daemon boundary. They are deliberately not JSON fields.
	CreatorSessionID string `json:"-"`
	CreatorOwnerID   string `json:"-"`
	// DelegationKind records whether a child was explicitly requested by a
	// person or created by its parent agent. It is presentation provenance,
	// not an authorization principal.
	DelegationKind string `json:"delegationKind,omitempty"`
	// Permissions is resolved by the daemon. "inherit" is accepted only for a
	// child and can never give that child more access than its parent unless the
	// user explicitly enabled autonomous delegated work.
	Permissions string `json:"permissions,omitempty"`
	// Lifecycle is "task" for a worker that should close after a successful
	// final response, or "session" for a long-lived conversation.
	Lifecycle string `json:"lifecycle,omitempty"`
	// DisplayParentSessionID is copied only by trusted recovery code. It is
	// never accepted from the public create-session JSON body.
	DisplayParentSessionID *string              `json:"-"`
	ConversationID         string               `json:"-"`
	ConfigDir              string               `json:"-"`
	WorktreePath           string               `json:"-"`
	WorktreeBranch         string               `json:"-"`
	WorktreeBase           string               `json:"-"`
	SourceRepo             string               `json:"-"`
	Continuation           *ContinuationContext `json:"-"`
}

// EndSessionRequest is durable audit context for an explicit termination.
// Initiator fields are derived from the authenticated transport, not accepted
// from the public JSON body.
type EndSessionRequest struct {
	InitiatorKind string
	InitiatorID   string
	InitiatorName string
	Client        string
	Reason        string
	OperationID   string
}

// InputAttribution is trusted transport provenance for one text submission.
// It is never accepted from the JSON body.
type InputAttribution struct {
	SourceSessionID string
	Client          string
}

// ClaudeSessionOptions are explicit per-session overrides. Empty fields use
// the persisted Sessions default; "inherit" asks Sessions to defer to Claude
// itself for that setting.
type ClaudeSessionOptions struct {
	RemoteControl           string `json:"remoteControl,omitempty"`
	PermissionMode          string `json:"permissionMode,omitempty"`
	Model                   string `json:"model,omitempty"`
	Effort                  string `json:"effort,omitempty"`
	Chrome                  string `json:"chrome,omitempty"`
	SomewhereMCP            string `json:"somewhereMcp,omitempty"`
	RemoteControlNamePrefix string `json:"remoteControlNamePrefix,omitempty"`
}

type ClaudeEventsWindow struct {
	Events     []json.RawMessage
	NextIndex  int64
	TotalCount int64
	StartIndex int64
	EndIndex   int64
}
