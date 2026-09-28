package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type restartRequest struct {
	SourceSessionID  string `json:"sourceSessionId"`
	ConfirmSessionID string `json:"confirmSessionId"`
	Permissions      string `json:"permissions"`
	RemoteControl    bool   `json:"remoteControl"`
	RuntimeMode      string `json:"runtimeMode,omitempty"`
}

type restartReceipt struct {
	Request     restartRequest    `json:"request"`
	Source      state.SessionInfo `json:"source"`
	Adoption    recovery.Adoption `json:"adoption"`
	RuntimeMode string            `json:"runtimeMode"`
	OperationID string            `json:"operationId"`
	SourceEnded bool              `json:"sourceEnded"`
}

type restartResult struct {
	OK              bool                  `json:"ok"`
	Partial         bool                  `json:"partial,omitempty"`
	SourceSessionID string                `json:"sourceSessionId"`
	SourceEnded     bool                  `json:"sourceEnded"`
	LaneID          string                `json:"laneId,omitempty"`
	OperationID     string                `json:"operationId"`
	Error           string                `json:"error,omitempty"`
	Adoption        *recovery.AdoptResult `json:"adoption,omitempty"`
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request, origin string) {
	var body restartRequest
	if err := readJSON(r, &body); err != nil {
		s.sendJSON(w, 400, map[string]any{"error": err.Error()}, origin)
		return
	}
	if body.SourceSessionID == "" || body.ConfirmSessionID != body.SourceSessionID {
		s.sendJSON(w, 400, map[string]any{"error": "confirmSessionId must identify the exact source runtime to end"}, origin)
		return
	}
	if body.Permissions != state.PermissionsFull && body.Permissions != state.PermissionsConstrained {
		s.sendJSON(w, 400, map[string]any{"error": "choose constrained or full permissions explicitly"}, origin)
		return
	}
	recoveryMutationMu.Lock()
	defer recoveryMutationMu.Unlock()
	receipt, err := s.prepareRestart(body)
	if err != nil {
		s.sendJSON(w, 409, map[string]any{"error": err.Error()}, origin)
		return
	}
	end, err := captureEndRequest(r, "Restart the same conversation with explicitly selected permissions.", receipt.OperationID)
	if err != nil {
		s.sendJSON(w, 400, map[string]any{"error": err.Error()}, origin)
		return
	}
	// Finish the bounded operation even if the requesting client disconnects.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result := s.performRestart(ctx, receipt, end)
	status := http.StatusOK
	if !result.OK {
		status = http.StatusAccepted
	}
	s.sendJSON(w, status, result, origin)
}

func restartOperationID(source string) string {
	sum := sha256.Sum256([]byte("sessions-restart:" + source))
	sum[6] = (sum[6] & 15) | 64
	sum[8] = (sum[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

func (s *Server) prepareRestart(body restartRequest) (restartReceipt, error) {
	operation := restartOperationID(body.SourceSessionID)
	path := s.restartReceiptPath(operation)
	var receipt restartReceipt
	encoded, err := os.ReadFile(path)
	if err == nil {
		if err = json.Unmarshal(encoded, &receipt); err != nil {
			return receipt, fmt.Errorf("read restart receipt: %w", err)
		}
		if receipt.Request != body {
			return receipt, errors.New("this runtime already has a restart with different choices; retry the original choices, then inspect its replacement")
		}
		return receipt, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return receipt, err
	}
	source, found := s.registry.Get(body.SourceSessionID)
	if !found || source.HasExited() {
		return receipt, errors.New("source runtime is no longer live; use Resume for its saved conversation")
	}
	info := source.Info()
	if info.Tool != "claude-code" && info.Tool != "codex" {
		return receipt, errors.New("restart requires a Claude or Codex conversation")
	}
	uuid, _ := ledger.ExistingProviderResume(info.Cmd, info.Args)
	if uuid == "" {
		uuid = info.ConversationID
	}
	if uuid == "" {
		uuid = info.ClaudeSessionID
	}
	options := recovery.AdoptionOptions{}
	if info.ConfigDir != "" {
		if info.Tool == "claude-code" {
			options.ClaudeProjectsDir = filepath.Join(info.ConfigDir, "projects")
		} else {
			options.CodexSessionsDir = filepath.Join(info.ConfigDir, "sessions")
		}
	}
	adoption, err := recovery.ResolveAdoption(uuid, options)
	if err != nil {
		return receipt, fmt.Errorf("conversation cannot be resumed; source is still running: %w", err)
	}
	adoption.Cwd = info.Cwd
	mode, err := s.restartRuntimeMode(info, body.RemoteControl, body.RuntimeMode)
	if err != nil {
		return receipt, err
	}
	receipt = restartReceipt{Request: body, Source: info, Adoption: adoption, RuntimeMode: mode, OperationID: operation}
	return receipt, writeRestartReceipt(path, receipt)
}

func (s *Server) restartRuntimeMode(info state.SessionInfo, remoteControl bool, requested string) (string, error) {
	mode := "terminal"
	if info.Kind == state.KindClaudeStructured || info.Kind == state.KindCodexAppServer {
		mode = "rich"
	}
	if requested != "" {
		if requested != "terminal" && requested != "rich" {
			return "", errors.New("runtimeMode must be terminal or rich")
		}
		mode = requested
	}
	if remoteControl {
		if info.Tool != "claude-code" {
			return "", errors.New("Remote Control is supported only by Claude")
		}
		settings, err := state.LoadSettings(s.config.SettingsPath)
		if err != nil {
			return "", err
		}
		if settings.EffectiveOnboarding().RemoteControl != state.RemoteControlConsentEnabled {
			return "", errors.New("enable Remote Control in Settings first; source is still running")
		}
		mode = "terminal"
	}
	return mode, nil
}

func (s *Server) restartReceiptPath(operation string) string {
	return filepath.Join(s.config.StateRoot, "restarts", operation+".json")
}

func writeRestartReceipt(path string, receipt restartReceipt) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".restart-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(encoded); err == nil {
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

func (s *Server) performRestart(ctx context.Context, receipt restartReceipt, end state.EndSessionRequest) restartResult {
	result := restartResult{SourceSessionID: receipt.Source.ID, OperationID: receipt.OperationID, SourceEnded: receipt.SourceEnded}
	store, err := ledger.Open(ctx, ledger.Options{})
	if err != nil {
		result.Error = "ledger is unavailable; no runtime was ended or started by this attempt: " + err.Error()
		return result
	}
	defer store.Close()
	if err := s.confirmRestartEnd(ctx, &receipt, end); err != nil {
		result.SourceEnded = receipt.SourceEnded
		result.Partial = true
		result.Error = err.Error()
		return result
	}
	result.SourceEnded = true
	result.Partial = true
	claude := restartClaudeOptions(receipt)
	adopted, err := recovery.Adopt(ctx, receipt.Adoption, receipt.Source.Name, s.registry, store.Boundaries(), store.Observations(), recovery.AdoptOptions{
		OperationID: receipt.OperationID, Source: adoptSourceFromSession(receipt.Source), Events: store,
		RuntimeMode: receipt.RuntimeMode, Permissions: receipt.Request.Permissions, Claude: claude, Model: receipt.Source.Model, Effort: receipt.Source.Effort,
	})
	result.LaneID = adopted.LaneID
	if err != nil {
		var replay *state.StartCreateReplayError
		if errors.As(err, &replay) {
			result.LaneID = replay.SessionID
		}
		result.Error = "source ended; retry these same choices to recover the recorded replacement without creating another runtime: " + err.Error()
		return result
	}
	result.Adoption = &adopted
	result.Partial = adopted.Partial
	result.OK = !adopted.Partial
	return result
}

func (s *Server) confirmRestartEnd(ctx context.Context, receipt *restartReceipt, end state.EndSessionRequest) error {
	source, found := s.registry.Get(receipt.Source.ID)
	if receipt.SourceEnded {
		if found && !source.HasExited() {
			return errors.New("the original runtime is live again; no runtime was ended or created by this retry. Inspect its status before choosing a new operation")
		}
		return nil
	}
	if !found {
		return errors.New("source termination is not confirmed: the runtime is not attached yet. No replacement was started. Wait for discovery and retry these same choices")
	}
	if !source.HasExited() {
		var err error
		if killer, ok := s.registry.(attributedKillService); ok {
			err = killer.RequestKillAttributed(ctx, receipt.Source.ID, true, end)
		} else {
			err = s.registry.RequestKill(ctx, receipt.Source.ID, true)
		}
		if err != nil {
			return fmt.Errorf("could not confirm source termination: %w", err)
		}
		if err = waitRestartExit(ctx, source); err != nil {
			return err
		}
	}
	receipt.SourceEnded = true
	if err := writeRestartReceipt(s.restartReceiptPath(receipt.OperationID), *receipt); err != nil {
		return fmt.Errorf("source ended but its completion receipt could not be saved; no replacement started. Restore state storage and retry: %w", err)
	}
	return nil
}

func restartClaudeOptions(receipt restartReceipt) *state.ClaudeSessionOptions {
	if receipt.Source.Tool != "claude-code" {
		return nil
	}
	mode := state.ClaudePermissionManual
	if receipt.Request.Permissions == state.PermissionsFull {
		mode = state.ClaudePermissionBypass
	}
	remote := state.ClaudeChoiceOff
	if receipt.Request.RemoteControl {
		remote = state.ClaudeChoiceOn
	}
	return &state.ClaudeSessionOptions{PermissionMode: mode, RemoteControl: remote, Model: receipt.Source.Model, Effort: receipt.Source.Effort}
}

func waitRestartExit(ctx context.Context, source *state.Session) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for !source.HasExited() {
		select {
		case <-ctx.Done():
			return errors.New("source termination is not confirmed; no replacement started. Retry the same restart choices")
		case <-ticker.C:
		}
	}
	return nil
}
