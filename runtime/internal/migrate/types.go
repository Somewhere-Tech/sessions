// Package migrate implements resume-elsewhere transfers. It copies provider
// conversation state and a safe resume recipe; it never migrates a process or
// copies a Git worktree byte-for-byte.
package migrate

import (
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

const (
	MaxConversationBytes int64 = 64 << 20
	MaxReceiveBodyBytes  int64 = 88 << 20
	RuntimeRich                = "rich"
	RuntimeTerminal            = "terminal"
)

// SourceSession is the daemon and ledger metadata needed to prepare a move.
type SourceSession struct {
	ID        string
	Name      string
	Tool      string
	Cmd       string
	Args      []string
	Cwd       string
	CreatedAt int64
}

// Workspace describes the Git identity the destination must provide. Git
// bytes are not included in the transfer.
type Workspace struct {
	Git           bool   `json:"git"`
	Root          string `json:"root,omitempty"`
	RemoteName    string `json:"remote_name,omitempty"`
	RemoteURL     string `json:"remote_url,omitempty"`
	Branch        string `json:"branch,omitempty"`
	Revision      string `json:"revision,omitempty"`
	CheckpointRef string `json:"checkpoint_ref,omitempty"`
	Dirty         bool   `json:"dirty,omitempty"`
}

type WorkspaceOptions struct {
	AllowDirty bool
	DryRun     bool
	Now        func() time.Time
}

// ReceiveRequest is the authenticated daemon-to-daemon handoff. Byte slices
// use JSON's base64 encoding on the wire.
type ReceiveRequest struct {
	Tool              string    `json:"tool"`
	UUID              string    `json:"uuid,omitempty"`
	Cwd               string    `json:"cwd"`
	ConversationBytes []byte    `json:"conversation_bytes,omitempty"`
	ResumeRecipe      []string  `json:"resume_recipe"`
	RuntimeMode       string    `json:"runtime_mode,omitempty"`
	Name              string    `json:"name,omitempty"`
	SourceID          string    `json:"source_id,omitempty"`
	SourceEndpoint    string    `json:"source_endpoint,omitempty"`
	Workspace         Workspace `json:"workspace"`
}

type ReceiveResult struct {
	OK                bool   `json:"ok"`
	ConversationPath  string `json:"conversation_path,omitempty"`
	ConversationBytes int    `json:"conversation_bytes"`
	AlreadyPresent    bool   `json:"already_present,omitempty"`
	WorkspaceReady    bool   `json:"workspace_ready"`
}

type CreateRequest struct {
	Tool           string   `json:"tool"`
	UUID           string   `json:"uuid"`
	Cwd            string   `json:"cwd"`
	ResumeRecipe   []string `json:"resume_recipe"`
	RuntimeMode    string   `json:"runtime_mode,omitempty"`
	Name           string   `json:"name,omitempty"`
	SourceID       string   `json:"source_id"`
	SourceEndpoint string   `json:"source_endpoint"`
}

type CreateResult struct {
	Session         state.SessionInfo `json:"session"`
	LineageRecorded bool              `json:"lineage_recorded"`
	Warning         string            `json:"warning,omitempty"`
}

type MoveResult struct {
	SourceID              string        `json:"source_id"`
	TargetID              string        `json:"target_id,omitempty"`
	TargetEndpoint        string        `json:"target_endpoint"`
	Tool                  string        `json:"tool"`
	Cwd                   string        `json:"cwd"`
	ResumeRecipe          []string      `json:"resume_recipe"`
	RuntimeMode           string        `json:"runtime_mode"`
	Workspace             Workspace     `json:"workspace"`
	ConversationSize      int           `json:"conversation_bytes"`
	DryRun                bool          `json:"dry_run"`
	Receive               ReceiveResult `json:"receive,omitempty"`
	TargetLineageRecorded bool          `json:"target_lineage_recorded,omitempty"`
	Warning               string        `json:"warning,omitempty"`
}

type ReceiveOptions struct {
	Home string
	Now  func() time.Time
}

// ExportRequest is evaluated on the source daemon. The client then carries
// the returned handoff to the destination, so neither daemon ever receives
// the other daemon's long-lived device credential.
type ExportRequest struct {
	SessionID      string `json:"session_id"`
	SourceEndpoint string `json:"source_endpoint"`
	RuntimeMode    string `json:"runtime_mode,omitempty"`
	DryRun         bool   `json:"dry_run,omitempty"`
	AllowDirty     bool   `json:"allow_dirty,omitempty"`
}

type ExportResult struct {
	Request ReceiveRequest `json:"request"`
	Plan    MoveResult     `json:"plan"`
}

type CompleteRequest struct {
	SourceID       string `json:"source_id"`
	TargetEndpoint string `json:"target_endpoint"`
	TargetID       string `json:"target_id"`
	CheckpointRef  string `json:"checkpoint_ref,omitempty"`
}
