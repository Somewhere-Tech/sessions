// Package state owns the runner state-directory layout and persistent event
// framing shared with the TypeScript implementation.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/somewhere-tech/sessions/runtime/internal/ipc"
)

// Paths is the complete on-disk file group for one runner.
type Paths struct {
	Dir          string
	ID           string
	Socket       string
	Meta         string
	Events       string
	Log          string
	Manifest     string
	Structured   string
	ClaudeP      string
	Continuation string
}

func For(dir, id string) Paths {
	base := filepath.Join(dir, id)
	return Paths{
		Dir:          dir,
		ID:           id,
		Socket:       ipc.RunnerEndpoint(dir, id),
		Meta:         base + ".json",
		Events:       base + ".events",
		Log:          base + ".log",
		Manifest:     base + ".manifest.json",
		Structured:   base + ".codexapp.jsonl",
		ClaudeP:      base + ".claudep.jsonl",
		Continuation: base + ".continuation.json",
	}
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

// Metadata mirrors runtime/testdata/node-runtime/src/runner.ts SessionMeta. Field order is kept the
// same so the human-readable JSON also matches the normative implementation.
type Metadata struct {
	ID                     string            `json:"id"`
	Name                   string            `json:"name,omitempty"`
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
	DelegationKind         string            `json:"delegation_kind,omitempty"`
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

func WriteMetadata(path string, meta Metadata) error {
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
