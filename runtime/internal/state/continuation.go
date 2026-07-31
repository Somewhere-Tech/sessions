package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	ContinuationSchemaVersion = 1
	ContinuationNativeImport  = "native-import"
	ContinuationLinkedSearch  = "linked-search"
	maxContinuationBytes      = 8 * 1024 * 1024
)

// ContinuationMessage is the deliberately small provider-neutral history
// surface. Only authored conversation text crosses providers; tool calls,
// command output, diffs, credentials, and provider-internal records stay in
// the source store.
type ContinuationMessage struct {
	Role      string `json:"role"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp,omitempty"`
}

// ContinuationContext is a local, mode-0600 sidecar consumed by the newly
// created runner. Source history remains untouched and searchable by its
// stable Sessions history ID.
type ContinuationContext struct {
	SchemaVersion       int                   `json:"schemaVersion"`
	SourceHistoryID     string                `json:"sourceHistoryId"`
	SourceProvider      string                `json:"sourceProvider"`
	SourceProviderID    string                `json:"sourceProviderId,omitempty"`
	SourceTitle         string                `json:"sourceTitle,omitempty"`
	SourceCWD           string                `json:"sourceCwd"`
	SourceWorktreePath  string                `json:"sourceWorktreePath,omitempty"`
	SourceBranch        string                `json:"sourceBranch,omitempty"`
	SourceRepo          string                `json:"sourceRepo,omitempty"`
	DestinationProvider string                `json:"destinationProvider"`
	Mode                string                `json:"mode"`
	Fork                bool                  `json:"fork,omitempty"`
	Messages            []ContinuationMessage `json:"messages"`
	LocalHistoryReady   bool                  `json:"localHistoryReady,omitempty"`
	ProviderContext     string                `json:"providerContext,omitempty"`
}

func (c ContinuationContext) Validate() error {
	if c.SchemaVersion != ContinuationSchemaVersion {
		return fmt.Errorf("unsupported continuation schema %d", c.SchemaVersion)
	}
	if strings.TrimSpace(c.SourceHistoryID) == "" {
		return errors.New("continuation source history id is required")
	}
	if c.SourceProvider != "claude" && c.SourceProvider != "codex" {
		return fmt.Errorf("unsupported continuation source provider %q", c.SourceProvider)
	}
	if c.DestinationProvider != "claude" && c.DestinationProvider != "codex" {
		return fmt.Errorf("unsupported continuation destination provider %q", c.DestinationProvider)
	}
	if c.SourceProvider == c.DestinationProvider && !c.Fork {
		return errors.New("cross-provider continuation requires different source and destination providers")
	}
	if c.Mode != ContinuationNativeImport && c.Mode != ContinuationLinkedSearch {
		return fmt.Errorf("unsupported continuation mode %q", c.Mode)
	}
	if len(c.Messages) == 0 {
		return errors.New("continuation has no authored messages")
	}
	for index, message := range c.Messages {
		if message.Role != "user" && message.Role != "assistant" {
			return fmt.Errorf("continuation message %d has unsupported role %q", index, message.Role)
		}
		if strings.TrimSpace(message.Text) == "" {
			return fmt.Errorf("continuation message %d is empty", index)
		}
	}
	return nil
}

func WriteContinuation(path string, continuation ContinuationContext) error {
	encoded, err := continuationBytes(continuation)
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
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

func continuationBytes(continuation ContinuationContext) ([]byte, error) {
	if err := continuation.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.MarshalIndent(continuation, "", "  ")
	if err != nil {
		return nil, err
	}
	if len(encoded) > maxContinuationBytes {
		return nil, fmt.Errorf(
			"authored conversation is too large to import safely (%d bytes; limit %d)",
			len(encoded), maxContinuationBytes,
		)
	}
	return encoded, nil
}

func ReadContinuation(path string) (ContinuationContext, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return ContinuationContext{}, err
	}
	if len(encoded) > maxContinuationBytes {
		return ContinuationContext{}, fmt.Errorf("continuation sidecar exceeds %d bytes", maxContinuationBytes)
	}
	var continuation ContinuationContext
	if err := json.Unmarshal(encoded, &continuation); err != nil {
		return ContinuationContext{}, err
	}
	if err := continuation.Validate(); err != nil {
		return ContinuationContext{}, err
	}
	return continuation, nil
}
