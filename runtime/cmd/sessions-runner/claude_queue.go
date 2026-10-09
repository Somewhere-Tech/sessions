package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
)

const claudeQueueLimit = 32
const claudeQueueBytes = 1024 * 1024

type claudeQueuedMessage struct {
	ID       string `json:"operation_id"`
	Text     string `json:"text,omitempty"`
	Hash     string `json:"hash"`
	Phase    string `json:"phase"`
	Boundary string `json:"boundary"`
	At       string `json:"timestamp"`
}

type claudeMessageQueue struct {
	Version int                   `json:"version"`
	Paused  bool                  `json:"paused"`
	Items   []claudeQueuedMessage `json:"items"`
}

// queueMu serializes admission, disk commits and claims, never provider IO.
// A claimed message is never replayed on runner restart: its delivery may
// already have happened. Only still-queued messages may be explicitly resumed.
func (r *claudeStructuredRunner) loadMessageQueue() error {
	data, err := os.ReadFile(r.paths.MessageQueue)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Claude next-turn queue: %w", err)
	}
	if err := json.Unmarshal(data, &r.queue); err != nil || r.queue.Version != 1 {
		return errors.New("cannot read Claude next-turn queue; preserve it and inspect the conversation before recovery")
	}
	pending := false
	seen := r.queueHistoryIDs()
	for _, item := range r.queue.Items {
		if item.Phase != "queued" && item.Phase != "dispatched" && item.Phase != "done" {
			return errors.New("invalid Claude queue entry; preserve the queue before recovery")
		}
		if item.ID == "" || (item.Boundary != "queue" && item.Boundary != "runner") ||
			(item.Phase != "done" && (item.Text == "" || item.Hash != claudeMessageHash(item.Text))) {
			return errors.New("invalid saved Claude message; preserve the queue and inspect the conversation before recovery")
		}
		pending = pending || item.Phase != "done"
		if item.Phase == "queued" && !seen["sessions-message:"+item.ID] {
			r.appendQueueEvent(item)
		}
		if item.Phase == "dispatched" && !seen["sessions-message:"+item.ID+":unknown"] {
			r.appendQueueUnknown(item)
		}
	}
	if pending {
		r.queue.Paused = true
		r.queueNotice("Saved next-turn messages are paused after runner restart. Check the conversation, then use Retry to continue only messages not yet dispatched. Dispatched messages will not be resent.")
	}
	return nil
}

func (r *claudeStructuredRunner) queueHistoryIDs() map[string]bool {
	seen := make(map[string]bool)
	for _, raw := range r.history {
		var event struct {
			UUID string `json:"uuid"`
		}
		if json.Unmarshal(raw, &event) == nil {
			seen[event.UUID] = true
		}
	}
	return seen
}

func writeClaudeQueue(path string, queue claudeMessageQueue) error {
	data, err := json.Marshal(queue)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".claude-queue-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
}

func (r *claudeStructuredRunner) admitClaudeMessage(control proto.MessageControl) proto.MessageResult {
	result := proto.MessageResult{OperationID: control.OperationID}
	r.queueMu.Lock()
	defer r.queueMu.Unlock()
	if err := proto.ValidateMessage(control); err != nil {
		result.Error = err.Error()
		return result
	}
	if r.ctx.Err() != nil {
		result.Error = "Claude runner is stopping. This message was not queued."
		return result
	}
	if prior, found := r.queuedReceipt(control, &result); found {
		return prior
	}
	r.mu.Lock()
	active := r.active
	r.mu.Unlock()
	boundary := "runner"
	if active || r.queue.Paused {
		boundary = "queue"
	}
	item := claudeQueuedMessage{ID: control.OperationID, Text: control.Text, Hash: claudeMessageHash(control.Text),
		Phase: "queued", Boundary: boundary, At: time.Now().UTC().Format(time.RFC3339Nano)}
	next, err := r.queueWith(item)
	if err == nil {
		err = writeClaudeQueue(r.paths.MessageQueue, next)
	}
	if err != nil {
		result.Error = "Message was not queued: " + err.Error() + ". Keep your draft and try again after fixing storage."
		return result
	}
	r.queue = next
	r.appendQueueEvent(item)
	result.Accepted, result.Boundary = true, boundary
	if r.queue.Paused {
		result.Error = "Saved for the next turn, but the queue is paused. Resolve the failed turn or inspect the conversation and use Retry; do not resend this message."
	}
	_, err = r.claimQueuedTurnLocked(false)
	if err != nil {
		result.Boundary = "queue"
		result.Error = "Message is saved, but could not start: " + err.Error() + ". Fix storage, then use Retry; do not resend."
	}
	return result
}

func (r *claudeStructuredRunner) claimQueuedTurnLocked(resume bool) (bool, error) {
	r.mu.Lock()
	if r.active || r.ctx.Err() != nil || (r.queue.Paused && !resume) {
		r.mu.Unlock()
		return false, nil
	}
	// Reserve the turn briefly, then perform storage IO without holding mu.
	// HELLO/status and active-turn interruption must remain responsive.
	r.active = true
	r.mu.Unlock()
	for index, item := range r.queue.Items {
		if item.Phase != "queued" {
			continue
		}
		next := r.queue
		next.Items = append([]claudeQueuedMessage(nil), next.Items...)
		next.Items[index].Phase, next.Paused = "dispatched", false
		if err := writeClaudeQueue(r.paths.MessageQueue, next); err != nil {
			r.queue.Paused = true
			r.mu.Lock()
			r.active = false
			r.mu.Unlock()
			return false, err
		}
		r.queue, r.queueCurrent = next, item.ID
		if r.retry != nil {
			r.retry.Replace()
		}
		go r.runTurn(item.Text, 0, true, item.ID)
		return true, nil
	}
	r.mu.Lock()
	r.active = false
	r.mu.Unlock()
	return false, nil
}

func (r *claudeStructuredRunner) finishClaudeTurn(id string, successful bool) {
	r.queueMu.Lock()
	defer r.queueMu.Unlock()
	r.mu.Lock()
	r.active, r.turnCancel = false, nil
	r.mu.Unlock()
	next := r.queue
	next.Items = append([]claudeQueuedMessage(nil), next.Items...)
	next.Paused = !successful
	for index := range next.Items {
		if next.Items[index].ID == id && successful {
			next.Items[index].Phase, next.Items[index].Text = "done", ""
		}
	}
	next.Items = trimClaudeQueueReceipts(next.Items)
	if len(next.Items) > 0 {
		if err := writeClaudeQueue(r.paths.MessageQueue, next); err != nil {
			r.queue.Paused = true
			r.queueNotice("Next-turn messages are paused because queue state could not be saved: " + err.Error() + ". Fix storage and inspect the conversation before using Retry. Nothing was resent.")
			return
		}
	}
	r.queue = next
	if successful {
		r.queueCurrent = ""
	}
	if successful {
		if _, err := r.claimQueuedTurnLocked(false); err != nil {
			r.queueNotice("Saved next-turn messages could not start: " + err.Error() + ". Fix storage, then use Retry; do not resend.")
		}
	} else if len(next.Items) > 0 {
		r.queueNotice("Next-turn messages remain saved. The current turn failed or was interrupted; resolve it before continuing the queue with Retry.")
	}
}

func (r *claudeStructuredRunner) resumeClaudeQueue() (bool, error) {
	r.queueMu.Lock()
	defer r.queueMu.Unlock()
	return r.claimQueuedTurnLocked(true)
}
