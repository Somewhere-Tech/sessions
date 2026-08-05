package state

import (
	"encoding/json"
	"os"
)

// WriteRunnerMetadata persists the runner-owned view of one session without
// discarding the daemon-owned fields already on disk.
//
// A runner rebuilds Metadata from its launch configuration, which never
// carries tags, display grouping, set-aside, delegation, permission mode, or
// lifecycle. WriteMetadata replaces the whole document, so a plain runner
// write — at startup, or when the user changes the model mid-session — used to
// erase those fields. Nothing failed visibly: the live daemon kept its
// in-memory copy, and the loss only surfaced when the next daemon restart
// re-read the file and a task session came back as a plain session.
//
// The daemon owns those fields end to end, so the on-disk values always win.
// A runner has no way to express them and therefore no way to intend a change.
func WriteRunnerMetadata(path string, meta Metadata) error {
	return WriteMetadata(path, MergeRunnerMetadata(readMetadataForMerge(path), meta))
}

// MergeRunnerMetadata layers a runner-owned document over the daemon-owned
// fields of the document already on disk.
func MergeRunnerMetadata(existing, next Metadata) Metadata {
	merged := next
	merged.Tags = CloneTags(existing.Tags)
	merged.DisplayParentSessionID = cloneStringPointer(existing.DisplayParentSessionID)
	merged.SetAsideAt = cloneInt64Pointer(existing.SetAsideAt)
	merged.DelegationKind = existing.DelegationKind
	merged.Permissions = existing.Permissions
	merged.Lifecycle = existing.Lifecycle

	// Continuation lineage is written by the daemon at creation, but only the
	// structured runners carry it in their own configuration; the PTY runner
	// rebuilds metadata without it. Preferring the on-disk value keeps a
	// PTY-backed continuation's lineage, while falling back to the incoming
	// value keeps the structured runners correct on a first write when no
	// document exists yet.
	if existing.ContinuedFromHistoryID != "" {
		merged.ContinuedFromHistoryID = existing.ContinuedFromHistoryID
	}
	if existing.ContinuedFromProvider != "" {
		merged.ContinuedFromProvider = existing.ContinuedFromProvider
	}
	if existing.ContinuationMode != "" {
		merged.ContinuationMode = existing.ContinuationMode
	}
	if existing.ImportedMessageCount != 0 {
		merged.ImportedMessageCount = existing.ImportedMessageCount
	}
	return merged
}

// readMetadataForMerge treats an absent, unreadable, or undecodable document
// as "no daemon-owned fields to preserve" rather than as a write failure. The
// runner's metadata file is how the daemon finds its socket at all, and a
// runner that refused to start would be restarted by launchd's
// KeepAlive{SuccessfulExit:false} policy into a loop. Rewriting the file is
// what already lets a damaged one heal.
func readMetadataForMerge(path string) Metadata {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return Metadata{}
	}
	var existing Metadata
	if err := json.Unmarshal(encoded, &existing); err != nil {
		return Metadata{}
	}
	return existing
}
