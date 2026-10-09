package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

// A new runtime resumes provider context, but its own structured file is empty.
// Restore a bounded display window once; never import these records into the
// model or replay them as new user input. Existing runtime history wins.
func (r *codexAppRunner) prepareResumeHistory() error {
	if err := r.openHistory(); err != nil {
		return err
	}
	conversationID, _ := providerConversationIdentity(r.cfg)
	if conversationID == "" || len(r.history) != 0 {
		return nil
	}
	if err := r.restoreResumeHistory(conversationID); err != nil {
		r.logger.Printf("restore Codex display history: %v", err)
		raw, _ := json.Marshal(map[string]any{
			"type": "system", "subtype": "resume_history_unavailable",
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
			"detail":    "The conversation resumed, but its earlier display history could not be loaded. The previous runtime remains in saved conversations.",
		})
		r.appendStructured(raw)
	}
	return nil
}

func (r *codexAppRunner) restoreResumeHistory(conversationID string) error {
	root := ""
	if r.cfg.configDir != "" {
		root = filepath.Join(r.cfg.configDir, "sessions")
	}
	resolved := watch.ResolveCodexRolloutPath(watch.CodexResolveOptions{
		ConversationID: conversationID, SessionsDir: root,
	})
	if resolved.Path == "" {
		return fmt.Errorf("the exact provider transcript is unavailable")
	}
	file, err := os.Open(resolved.Path)
	if err != nil {
		return err
	}
	defer file.Close()
	// Reuse the runner's byte/record-limited tail reader. No full-transcript
	// scan, and no modification of the provider's append-only source.
	lines, err := readStructuredHistoryTail(file)
	if err != nil {
		return err
	}
	var history []json.RawMessage
	for index, line := range lines {
		var record map[string]any
		if json.Unmarshal(line, &record) != nil {
			continue
		}
		normalized := watch.NormalizeCodexRolloutLine(record, watch.CodexNormalizeContext{
			RolloutBasename: filepath.Base(resolved.Path), LineIndex: index,
		})
		for _, event := range normalized.Events {
			// The UI distinguishes imported display records from fresh app-server
			// turns, keeping earlier answers visible when live events arrive.
			event["source"] = "sessions-continuation"
			raw, err := json.Marshal(event)
			if err != nil {
				return err
			}
			history = retainStructuredEvent(history, raw)
		}
	}
	// The same append boundary as every live record, under the same lock, so
	// a partial import write cannot absorb the record written after it.
	r.streamMu.Lock()
	for _, event := range history {
		if err := appendStructuredRecord(r.historyFile, &r.historyEnd, event); err != nil {
			r.streamMu.Unlock()
			return err
		}
	}
	r.streamMu.Unlock()
	if err := r.historyFile.Sync(); err != nil {
		return err
	}
	r.history = history
	return nil
}
