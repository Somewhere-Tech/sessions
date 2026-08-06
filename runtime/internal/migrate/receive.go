package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

var providerUUIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

func Receive(ctx context.Context, request ReceiveRequest, options ReceiveOptions) (ReceiveResult, error) {
	if request.Cwd == "" || !filepath.IsAbs(request.Cwd) {
		return ReceiveResult{}, errors.New("cwd must be an absolute path")
	}
	if len(request.ResumeRecipe) == 0 || request.ResumeRecipe[0] == "" {
		return ReceiveResult{}, errors.New("resume_recipe is required")
	}
	if int64(len(request.ConversationBytes)) > MaxConversationBytes {
		return ReceiveResult{}, fmt.Errorf("conversation exceeds %d bytes", MaxConversationBytes)
	}
	tool := canonicalTool(request.Tool, request.ResumeRecipe[0])
	if tool != "claude-code" && tool != "codex" {
		if len(request.ConversationBytes) != 0 {
			return ReceiveResult{}, errors.New("terminal moves cannot include conversation bytes")
		}
		if err := EnsureWorkspace(ctx, request.Cwd, request.Workspace); err != nil {
			return ReceiveResult{}, err
		}
		return ReceiveResult{OK: true, WorkspaceReady: true}, nil
	}
	if !providerUUIDPattern.MatchString(request.UUID) {
		return ReceiveResult{}, errors.New("uuid is required for provider moves")
	}
	provider, safe := ledger.SafeResumeRecipe(tool, request.ResumeRecipe[0], request.ResumeRecipe[1:])
	if provider != request.UUID || !slices.Equal(safe, request.ResumeRecipe) {
		return ReceiveResult{}, errors.New("resume_recipe is not the minimal recipe for uuid")
	}
	if len(request.ConversationBytes) == 0 {
		return ReceiveResult{}, errors.New("conversation_bytes is required for provider moves")
	}
	var codexTime time.Time
	if tool == "codex" {
		var err error
		codexTime, err = codexConversationTime(request.ConversationBytes, request.UUID)
		if err != nil {
			return ReceiveResult{}, err
		}
	}
	if err := EnsureWorkspace(ctx, request.Cwd, request.Workspace); err != nil {
		return ReceiveResult{}, err
	}
	result := ReceiveResult{OK: true, ConversationBytes: len(request.ConversationBytes), WorkspaceReady: true}
	home := options.Home
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return ReceiveResult{}, fmt.Errorf("resolve target home: %w", err)
		}
	}
	var destination string
	if tool == "claude-code" {
		destination = claudeConversationDestination(filepath.Join(home, ".claude", "projects"), request.Cwd, request.UUID)
	} else {
		at := codexTime
		if at.IsZero() {
			now := time.Now
			if options.Now != nil {
				now = options.Now
			}
			at = now()
		}
		name := "rollout-" + at.UTC().Format("2006-01-02T15-04-05") + "-" + request.UUID + ".jsonl"
		destination = filepath.Join(home, ".codex", "sessions", at.Format("2006"), at.Format("01"), at.Format("02"), name)
	}
	already, err := writeNewConversation(destination, request.ConversationBytes)
	if err != nil {
		return ReceiveResult{}, err
	}
	result.ConversationPath = destination
	result.AlreadyPresent = already
	return result, nil
}

// claudeConversationDestination names the file a moved Claude conversation must
// land in for the destination machine to be able to resume it.
//
// The bucket has to be the one Claude Code itself computes, which folds EVERY
// non-alphanumeric character to a dash (watch.EncodeClaudeCWDStrict). The
// narrow encoding folds only separators, so for any workspace whose path
// contains a dot, underscore, space or other punctuation the two disagree and
// the conversation was written into a directory Claude never reads: the move
// reported success and native "claude --resume <uuid>" at the destination could
// not see the conversation at all. /Users/uzair/pretty_tmux is the whole bug --
// Claude reads -Users-uzair-pretty-tmux, Sessions wrote -Users-uzair-pretty_tmux.
//
// Writing into a bucket that several workspaces can share is safe here in a way
// it is not for resolution. The file is named by provider UUID, which is unique
// across workspaces, and this is exactly what Claude Code does with its own
// conversations. Attribution back to a workspace never depends on the directory
// name: the transcript records its own cwd, and readers already probe every
// encoding (watch.ClaudeProjectDirsUnder).
//
// An earlier Sessions wrote this conversation under the narrow encoding, so any
// candidate bucket that already holds this UUID wins over the strict one. That
// keeps a repeated move idempotent instead of leaving two copies of one
// conversation in two buckets -- a duplicate would make the UUID resolve to
// several Claude projects and break adoption by UUID outright.
//
// Known remaining gap, which belongs to watch's encoder and not to this
// function: Claude also caps the bucket name at 200 characters and appends a
// hash of the unencoded cwd past that (2.1.222, K2e=200):
//
//	function rA(e){let t=e.replace(/[^a-zA-Z0-9]/g,"-");
//	  if(t.length<=K2e)return t;
//	  return `${t.slice(0,K2e)}-${Math.abs(Fat(e)).toString(36)}`}
//
// EncodeClaudeCWDStrict does not truncate, so a workspace whose encoded path
// exceeds 200 characters still lands beside Claude's bucket. Reproducing the
// truncation here alone would be worse than not doing it: every Sessions reader
// probes watch's encodings, so a name only this function knows how to build is
// one no reader could find. It has to be fixed in watch, for readers and
// writers together.
func claudeConversationDestination(projects, cwd, uuid string) string {
	name := uuid + ".jsonl"
	destination := filepath.Join(projects, watch.EncodeClaudeCWDStrict(cwd), name)
	for _, dir := range watch.ClaudeProjectDirsUnder(projects, cwd) {
		existing := filepath.Join(dir, name)
		if existing == destination {
			continue
		}
		if info, err := os.Stat(existing); err == nil && info.Mode().IsRegular() {
			return existing
		}
	}
	return destination
}

func codexConversationTime(encoded []byte, uuid string) (time.Time, error) {
	line := encoded
	if index := bytes.IndexByte(encoded, '\n'); index >= 0 {
		line = encoded[:index]
	}
	var record struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Payload   struct {
			ID        string `json:"id"`
			Timestamp string `json:"timestamp"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(line, &record); err != nil {
		return time.Time{}, fmt.Errorf("decode Codex session_meta: %w", err)
	}
	if record.Type != "session_meta" || record.Payload.ID != uuid {
		return time.Time{}, errors.New("Codex conversation session_meta does not match uuid")
	}
	raw := record.Payload.Timestamp
	if raw == "" {
		raw = record.Timestamp
	}
	if raw == "" {
		return time.Time{}, nil
	}
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse Codex session timestamp: %w", err)
	}
	return at, nil
}

func writeNewConversation(path string, encoded []byte) (bool, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, fmt.Errorf("create conversation directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return false, fmt.Errorf("secure conversation directory: %w", err)
	}
	if existing, err := os.ReadFile(path); err == nil {
		if bytes.Equal(existing, encoded) {
			return true, nil
		}
		return false, fmt.Errorf("refusing to overwrite a different conversation: %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("inspect conversation destination: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		existing, readErr := os.ReadFile(path)
		if readErr == nil && bytes.Equal(existing, encoded) {
			return true, nil
		}
		return false, fmt.Errorf("refusing to overwrite a conversation created concurrently: %s", path)
	}
	if err != nil {
		return false, fmt.Errorf("create conversation: %w", err)
	}
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()
	if _, err := io.Copy(file, bytes.NewReader(encoded)); err != nil {
		return false, fmt.Errorf("write conversation: %w", err)
	}
	if err := file.Sync(); err != nil {
		return false, fmt.Errorf("sync conversation: %w", err)
	}
	if err := file.Close(); err != nil {
		return false, fmt.Errorf("close conversation: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return false, fmt.Errorf("secure conversation: %w", err)
	}
	keep = true
	return false, nil
}

func DecodeReceive(reader io.Reader, target *ReceiveRequest) error {
	decoder := json.NewDecoder(io.LimitReader(reader, MaxReceiveBodyBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request must contain one JSON value")
		}
		return err
	}
	return nil
}
