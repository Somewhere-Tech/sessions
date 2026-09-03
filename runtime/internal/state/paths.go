// Package state owns the runner state-directory layout and persistent event
// framing shared with the TypeScript implementation.
package state

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/filelock"
	"github.com/somewhere-tech/sessions/runtime/internal/ipc"
)

// Paths is the complete on-disk file group for one runner.
type Paths struct {
	Dir            string
	ID             string
	Socket         string
	Meta           string
	Events         string
	Log            string
	Manifest       string
	KeepAlive      string
	RestorePending string
	Structured     string
	ClaudeP        string
	Continuation   string
	// Transcript is Sessions' own append-only copy of the provider
	// conversation, and TranscriptMeta records where it came from. The
	// provider owns the original and prunes it on its own schedule; these are
	// what remain when it does.
	Transcript     string
	TranscriptMeta string
}

func For(dir, id string) Paths {
	base := filepath.Join(dir, id)
	return Paths{
		Dir:            dir,
		ID:             id,
		Socket:         ipc.RunnerEndpoint(dir, id),
		Meta:           base + ".json",
		Events:         base + ".events",
		Log:            base + ".log",
		Manifest:       base + ".manifest.json",
		KeepAlive:      base + ".keepalive.json",
		RestorePending: base + ".restore-pending.json",
		Structured:     base + ".codexapp.jsonl",
		ClaudeP:        base + ".claudep.jsonl",
		Continuation:   base + ".continuation.json",

		Transcript:     base + ".transcript.jsonl",
		TranscriptMeta: base + ".transcript.meta.json",
	}
}

// runnerSidecarJSONSuffixes are the non-metadata ".json" artifacts For() writes
// beside "<id>.json". Discovery on a host with no socket artifact keys off the
// metadata name, so every suffix added to Paths must be listed here or each
// session that has one produces a phantom runner id.
var runnerSidecarJSONSuffixes = []string{
	".manifest.json",
	".keepalive.json",
	".restore-pending.json",
	".continuation.json",
	".transcript.meta.json",
}

// RunnerIDFromMetadataName returns the runner id for a "<id>.json" metadata
// file and reports false for anything else in the runner state directory,
// including the sidecars listed above. A phantom id survives as a permanent
// join error out of Registry.Discover and buries the real lost-session signal,
// so this refuses rather than guesses.
func RunnerIDFromMetadataName(name string) (string, bool) {
	if !strings.HasSuffix(name, ".json") {
		return "", false
	}
	for _, suffix := range runnerSidecarJSONSuffixes {
		if strings.HasSuffix(name, suffix) {
			return "", false
		}
	}
	id := strings.TrimSuffix(name, ".json")
	// A leading dot is an in-progress WriteMetadata temp file, never an id.
	if id == "" || strings.HasPrefix(id, ".") {
		return "", false
	}
	return id, true
}

// DefaultRunnerDir implements ~/.local/state/sessions/runners without
// writing it. SESSIONS_STATE_DIR remains the launch-time override.
func DefaultRunnerDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "sessions", "runners"), nil
}

func EnsureDir(dir string) error {
	return os.MkdirAll(dir, 0o700)
}

// Metadata implements the stable SessionMeta shape. Field order is kept stable
// so human-readable on-disk diffs stay boring.
type Metadata struct {
	ID                     string            `json:"id"`
	Name                   string            `json:"name,omitempty"`
	NameSource             string            `json:"name_source,omitempty"`
	Description            string            `json:"description,omitempty"`
	DescriptionSource      string            `json:"description_source,omitempty"`
	Tags                   map[string]string `json:"tags,omitempty"`
	Kind                   string            `json:"kind,omitempty"`
	SpecPath               string            `json:"specPath,omitempty"`
	Profile                string            `json:"profile,omitempty"`
	ConfigDir              string            `json:"config_dir,omitempty"`
	Cmd                    string            `json:"cmd"`
	Args                   []string          `json:"args"`
	Cwd                    string            `json:"cwd"`
	Cols                   int               `json:"cols"`
	Rows                   int               `json:"rows"`
	CreatedAt              int64             `json:"createdAt"`
	PID                    int               `json:"pid"`
	SockPath               string            `json:"sockPath"`
	ConversationID         string            `json:"conversationId,omitempty"`
	RemoteEndpoint         string            `json:"remoteEndpoint,omitempty"`
	ClaudeSessionID        string            `json:"claudeSessionId,omitempty"`
	ContinuedFromHistoryID string            `json:"continuedFromHistoryId,omitempty"`
	ContinuedFromProvider  string            `json:"continuedFromProvider,omitempty"`
	ContinuationMode       string            `json:"continuationMode,omitempty"`
	ImportedMessageCount   int               `json:"importedMessageCount,omitempty"`
	DisplayParentSessionID *string           `json:"display_parent_session_id,omitempty"`
	SetAsideAt             *int64            `json:"set_aside_at,omitempty"`
	// Pinned is the user marking a workbench. It is omitted when false so an
	// older runtime that never learned the field decodes the document
	// unchanged and absence keeps meaning "not pinned", which is the same
	// compatibility shape set_aside_at uses for its cleared state.
	Pinned bool `json:"pinned,omitempty"`
	// LastHumanMessageAt and LastAgentMessageAt carry the daemon's
	// message-principal clocks across a restart. Both are omitted when absent,
	// so an older runtime decodes the document unchanged and absence keeps
	// meaning "nobody of that kind has spoken into this session yet" rather
	// than the epoch.
	LastHumanMessageAt *int64 `json:"last_human_message_at,omitempty"`
	LastAgentMessageAt *int64 `json:"last_agent_message_at,omitempty"`
	DelegationKind     string `json:"delegation_kind,omitempty"`
	Permissions        string `json:"permissions,omitempty"`
	Lifecycle          string `json:"lifecycle,omitempty"`
}

// CompletionManifest is the durable terminal fact emitted by a headless lane.
// FilesChanged is the number of Git-visible paths whose state changed between
// lane start and lane exit. It is absent when either snapshot is unavailable.
type CompletionManifest struct {
	ExitCode       int     `json:"exit_code"`
	Signal         *string `json:"signal"`
	DurationMS     int64   `json:"duration_ms"`
	LastOutputTail string  `json:"last_output_tail"`
	SpecPath       string  `json:"spec_path"`
	FilesChanged   *int    `json:"files_changed,omitempty"`
}

const metadataLockTimeout = 30 * time.Second

// WriteMetadata atomically replaces one metadata document while holding the
// same cross-process sidecar lock used by metadata read-modify-writes. Atomic
// rename prevents torn JSON; the lock prevents a daemon write and a separately
// launched runner write from silently replacing one another.
func WriteMetadata(path string, meta Metadata) error {
	return withMetadataLock(path, func() error {
		return writeMetadataUnlocked(path, meta)
	})
}

// updateMetadata holds the metadata lock across the read, mutation, and
// atomic replacement. Every daemon-owned field edit must use this helper; a
// lock acquired only for the final write is too late because another process
// may already have changed the document after the caller read it.
func updateMetadata(path string, mutate func(*Metadata) error) (Metadata, error) {
	var metadata Metadata
	err := withMetadataLock(path, func() error {
		encoded, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(encoded, &metadata); err != nil {
			return err
		}
		if err := mutate(&metadata); err != nil {
			return err
		}
		return writeMetadataUnlocked(path, metadata)
	})
	return metadata, err
}

func withMetadataLock(path string, operation func() error) error {
	ctx, cancel := context.WithTimeout(context.Background(), metadataLockTimeout)
	defer cancel()
	lock, err := filelock.Acquire(ctx, path+".lock")
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()
	if err := operation(); err != nil {
		return err
	}
	return lock.Release()
}

func writeMetadataUnlocked(path string, meta Metadata) error {
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	temporary, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	keepTemporary := true
	defer func() {
		_ = temporary.Close()
		if keepTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(b); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	keepTemporary = false
	return nil
}

func WriteCompletionManifest(path string, manifest CompletionManifest) error {
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(temporary, 0o600); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func ReadCompletionManifest(path string) (CompletionManifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return CompletionManifest{}, err
	}
	var manifest CompletionManifest
	if err := json.Unmarshal(b, &manifest); err != nil {
		return CompletionManifest{}, err
	}
	return manifest, nil
}
