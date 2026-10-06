package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/claudep"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func newClaudeQueueFixture(t *testing.T) (*claudeStructuredRunner, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("inert shell fixture requires POSIX sh")
	}
	root := t.TempDir()
	paths := state.For(root, "fixture")
	history, err := os.Create(paths.ClaudeP)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { history.Close() })
	logPath := filepath.Join(root, "provider-calls")
	command := filepath.Join(root, "fake-claude")
	script := "#!/bin/sh\nfor prompt do :; done\nprintf '%s\\n' \"$prompt\" >> \"$QUEUE_TEST_LOG\"\n" +
		"if [ \"$prompt\" = fail ]; then\nprintf '%s\\n' '{\"type\":\"result\",\"session_id\":\"fixture-claude\",\"is_error\":true,\"result\":\"permission denied\",\"subtype\":\"error\"}'\nexit 1\nfi\n" +
		"printf '%s\\n' '{\"type\":\"result\",\"session_id\":\"fixture-claude\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"done\"}'\n"
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	provider, err := claudep.NewClient(claudep.Options{ClaudePath: command, Env: []string{
		"HOME=" + root, "SESSIONS_STATE_DIR=" + root, "SESSIONS_LEDGER_PATH=" + filepath.Join(root, "ledger"),
		"SESSIONS_PORT=18999", "QUEUE_TEST_LOG=" + logPath,
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := &claudeStructuredRunner{paths: paths, ctx: ctx, client: provider, sessionID: "fixture-claude",
		historyFile: history, logger: log.New(io.Discard, "", 0), clients: make(map[*client]struct{}),
		cfg: config{cwd: root}, active: true}
	return r, logPath
}

func waitClaudeQueue(t *testing.T, r *claudeStructuredRunner, condition func(claudeMessageQueue) bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.queueMu.Lock()
		ok := condition(r.queue)
		r.queueMu.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	r.queueMu.Lock()
	defer r.queueMu.Unlock()
	t.Fatalf("inert provider did not reach expected queue state: %+v", r.queue)
}

func TestClaudeQueueSavesBusyMessagesAndRunsFIFOOnce(t *testing.T) {
	r, logPath := newClaudeQueueFixture(t)
	for _, text := range []string{"first", "second"} {
		control := proto.MessageControl{OperationID: text, Text: text}
		result := r.submitMessage(control)
		if !result.Accepted || result.Boundary != "queue" {
			t.Fatalf("receipt = %+v", result)
		}
		if repeat := r.submitMessage(control); !repeat.Accepted {
			t.Fatalf("repeat = %+v", repeat)
		}
	}
	info, err := os.Stat(r.paths.MessageQueue)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("queue file must be private: %v", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatal("provider launched before current turn completed")
	}
	r.finishClaudeTurn("", true)
	waitClaudeQueue(t, r, func(queue claudeMessageQueue) bool {
		return len(queue.Items) == 2 && queue.Items[0].Phase == "done" && queue.Items[1].Phase == "done"
	})
	data, err := os.ReadFile(logPath)
	if err != nil || string(data) != "first\nsecond\n" {
		t.Fatalf("provider calls = %q, %v", data, err)
	}
	if result := r.submitMessage(proto.MessageControl{OperationID: "first", Text: "different"}); result.Accepted {
		t.Fatal("reused operation id changed an accepted message")
	}
}

func TestClaudeQueueFailurePausesLaterMessages(t *testing.T) {
	r, logPath := newClaudeQueueFixture(t)
	for _, text := range []string{"fail", "later"} {
		if result := r.submitMessage(proto.MessageControl{OperationID: text, Text: text}); !result.Accepted {
			t.Fatal(result)
		}
	}
	r.finishClaudeTurn("", true)
	waitClaudeQueue(t, r, func(queue claudeMessageQueue) bool { return queue.Paused })
	data, _ := os.ReadFile(logPath)
	if string(data) != "fail\n" {
		t.Fatalf("failure launched queued follow-up: %q", data)
	}
	if result := r.submitMessage(proto.MessageControl{OperationID: "third", Text: "third"}); !result.Accepted || result.Boundary != "queue" || !strings.Contains(result.Error, "paused") {
		t.Fatalf("paused queue receipt = %+v", result)
	}
}

func TestClaudeQueueRestartNeverReplaysClaimedMessage(t *testing.T) {
	r, logPath := newClaudeQueueFixture(t)
	queue := claudeMessageQueue{Version: 1, Items: []claudeQueuedMessage{
		{ID: "uncertain", Text: "never resend", Phase: "dispatched", Hash: claudeMessageHash("never resend"), Boundary: "queue"},
		{ID: "pending", Text: "pending", Phase: "queued", Hash: claudeMessageHash("pending"), Boundary: "queue"},
	}}
	if err := writeClaudeQueue(r.paths.MessageQueue, queue); err != nil {
		t.Fatal(err)
	}
	r.active = false
	if err := r.loadMessageQueue(); err != nil {
		t.Fatal(err)
	}
	if !r.queue.Paused {
		t.Fatal("restart did not pause queue")
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatal("restart automatically launched work")
	}
	if started, err := r.resumeClaudeQueue(); !started || err != nil {
		t.Fatalf("resume = %v, %v", started, err)
	}
	waitClaudeQueue(t, r, func(queue claudeMessageQueue) bool { return queue.Items[1].Phase == "done" })
	data, _ := os.ReadFile(logPath)
	if string(data) != "pending\n" {
		t.Fatalf("replayed claimed prompt: %q", data)
	}
}

func TestClaudeQueueCannotAcknowledgeUnsavedMessage(t *testing.T) {
	r, _ := newClaudeQueueFixture(t)
	r.paths.MessageQueue = filepath.Join(t.TempDir(), "missing", "queue.json")
	result := r.submitMessage(proto.MessageControl{OperationID: "unsaved", Text: "keep draft"})
	if result.Accepted || result.Boundary != "" || !strings.Contains(result.Error, "not queued") || len(r.queue.Items) != 0 {
		t.Fatalf("save failure = %+v, queue = %+v", result, r.queue)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.history) != 0 {
		t.Fatal("unsaved message appeared as accepted in history")
	}
}

func TestClaudeQueueLimitsDoNotDropAcceptedMessages(t *testing.T) {
	r, _ := newClaudeQueueFixture(t)
	for index := range claudeQueueLimit {
		id := string(rune('A' + index))
		if result := r.submitMessage(proto.MessageControl{OperationID: id, Text: "saved"}); !result.Accepted {
			t.Fatal(result)
		}
	}
	result := r.submitMessage(proto.MessageControl{OperationID: "overflow", Text: "overflow"})
	if result.Accepted || len(r.queue.Items) != claudeQueueLimit {
		t.Fatalf("overflow = %+v", result)
	}
	data, err := os.ReadFile(r.paths.MessageQueue)
	var queue claudeMessageQueue
	if err != nil || json.Unmarshal(data, &queue) != nil || len(queue.Items) != claudeQueueLimit {
		t.Fatal("overflow altered durable queue")
	}
}

func TestClaudeQueueInterruptionAndStorageFailureNeverDrain(t *testing.T) {
	for _, storageFailure := range []bool{false, true} {
		r, logPath := newClaudeQueueFixture(t)
		if result := r.submitMessage(proto.MessageControl{OperationID: "saved", Text: "saved"}); !result.Accepted {
			t.Fatal(result)
		}
		if storageFailure {
			r.paths.MessageQueue = t.TempDir()
		}
		r.finishClaudeTurn("", storageFailure)
		if !r.queue.Paused {
			t.Fatal("interruption or failed queue commit did not pause")
		}
		if _, err := os.Stat(logPath); !os.IsNotExist(err) {
			t.Fatal("paused queue launched provider")
		}
	}
}

func TestClaudeQueueRejectsOversizeAndCorruptRecovery(t *testing.T) {
	r, _ := newClaudeQueueFixture(t)
	if result := r.submitMessage(proto.MessageControl{OperationID: "oversize", Text: strings.Repeat("x", claudeQueueBytes+1)}); result.Accepted {
		t.Fatal("oversized prompt accepted")
	}
	if err := os.WriteFile(r.paths.MessageQueue, []byte(`{"version":1,"items":[{"phase":"queued","operation_id":"corrupt","text":"changed","hash":"wrong","boundary":"queue"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.loadMessageQueue(); err == nil {
		t.Fatal("corrupted prompt was eligible for replay")
	}
}

func TestClaudeQueueRetainsBoundedCompletedReceipts(t *testing.T) {
	r, _ := newClaudeQueueFixture(t)
	for range 90 {
		r.queue.Items = append(r.queue.Items, claudeQueuedMessage{Phase: "done"})
	}
	r.finishClaudeTurn("", true)
	if len(r.queue.Items) != 64 {
		t.Fatalf("completed queue grew: %d", len(r.queue.Items))
	}
}

func TestClaudeQueueUndispatchedReceiptNeverClaimsTurnStarted(t *testing.T) {
	r, _ := newClaudeQueueFixture(t)
	r.queue.Items = []claudeQueuedMessage{{ID: "saved", Text: "saved", Hash: claudeMessageHash("saved"), Phase: "queued", Boundary: "runner"}}
	if result := r.submitMessage(proto.MessageControl{OperationID: "saved", Text: "saved"}); !result.Accepted || result.Boundary != "queue" {
		t.Fatalf("unclaimed duplicate was mislabeled as started: %+v", result)
	}
}
