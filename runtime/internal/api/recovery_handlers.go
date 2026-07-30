package api

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// Recovery mutations are serialized inside one daemon. Together with the
// provider UUID check this prevents concurrent --reopen/adopt requests from
// launching two copies of the same conversation.
var recoveryMutationMu sync.Mutex

func (s *Server) handleRecovery(response http.ResponseWriter, request *http.Request, corsOrigin string) {
	switch {
	case request.URL.Path == "/api/recovery" && request.Method == http.MethodGet:
		store, report, ok := s.openRecoveryReport(request.Context(), response, corsOrigin)
		if !ok {
			return
		}
		defer store.Close()
		s.sendJSON(response, http.StatusOK, report, corsOrigin)
	case request.URL.Path == "/api/recovery/reopen" && request.Method == http.MethodPost:
		var body struct {
			Force bool `json:"force,omitempty"`
		}
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		recoveryMutationMu.Lock()
		defer recoveryMutationMu.Unlock()
		store, report, ok := s.openRecoveryReport(request.Context(), response, corsOrigin)
		if !ok {
			return
		}
		defer store.Close()
		result := recovery.Reopen(request.Context(), report, s.registry, store.Observations(), recovery.ReopenOptions{Force: body.Force})
		s.sendJSON(response, http.StatusOK, result, corsOrigin)
	case request.URL.Path == "/api/recovery/fork" && request.Method == http.MethodPost:
		var body struct {
			SourceSessionID     string `json:"sourceSessionId"`
			DestinationProvider string `json:"destinationProvider,omitempty"`
			Name                string `json:"name,omitempty"`
		}
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		if strings.TrimSpace(body.SourceSessionID) == "" {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "sourceSessionId is required"}, corsOrigin)
			return
		}
		recoveryMutationMu.Lock()
		defer recoveryMutationMu.Unlock()
		sourceSession, live := s.registry.Get(body.SourceSessionID)
		if !live {
			s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "source session not found"}, corsOrigin)
			return
		}
		candidate := sourceSession.Info()
		if candidate.Exited {
			s.sendJSON(response, http.StatusConflict, map[string]any{
				"error": "source session has ended; use Continue conversation instead",
			}, corsOrigin)
			return
		}
		if candidate.Working {
			s.sendJSON(response, http.StatusConflict, map[string]any{
				"error": "wait for the current turn to finish before copying this conversation; the original is still running",
			}, corsOrigin)
			return
		}
		sourceProvider, providerErr := normalizeContinuationProvider(string(candidate.Tool))
		if providerErr != nil || sourceProvider == "" {
			s.sendJSON(response, http.StatusConflict, map[string]any{
				"error": "only live Claude and Codex conversations can be copied",
			}, corsOrigin)
			return
		}
		destination, destinationErr := normalizeContinuationProvider(body.DestinationProvider)
		if destinationErr != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": destinationErr.Error()}, corsOrigin)
			return
		}
		if destination == "" {
			destination = sourceProvider
		}
		sourceCandidates := s.registry.List(true)
		history, historyErr := s.integrationEndpoints.LookupHistory(sourceCandidates, candidate.ID)
		if historyErr != nil || !history.ConversationAvailable || history.PromptHistoryOnly {
			s.sendJSON(response, http.StatusConflict, map[string]any{
				"error": "a complete authored conversation snapshot is not available yet",
			}, corsOrigin)
			return
		}
		providerUUID, _ := ledger.ExistingProviderResume(candidate.Cmd, candidate.Args)
		if providerUUID == "" {
			providerUUID = candidate.ConversationID
		}
		if providerUUID == "" {
			providerUUID = candidate.ClaudeSessionID
		}
		if providerUUID == "" || providerUUID != history.ProviderSessionID {
			s.sendJSON(response, http.StatusConflict, map[string]any{
				"error": "source session history does not match its live provider conversation",
			}, corsOrigin)
			return
		}
		transcript, transcriptErr := s.integrationEndpoints.Transcript(sourceCandidates, history.ID)
		if transcriptErr != nil {
			s.sendJSON(response, http.StatusConflict, map[string]any{
				"error": "complete authored conversation is unavailable: " + transcriptErr.Error(),
			}, corsOrigin)
			return
		}
		messages := continuationMessages(transcript.Messages)
		mode := state.ContinuationLinkedSearch
		if destination == "codex" {
			mode = state.ContinuationNativeImport
		}
		continuation := state.ContinuationContext{
			SchemaVersion:   state.ContinuationSchemaVersion,
			SourceHistoryID: history.ID, SourceProvider: sourceProvider,
			SourceProviderID: history.ProviderSessionID, SourceTitle: history.Name,
			SourceCWD: history.CWD, DestinationProvider: destination,
			Mode: mode, Fork: true, Messages: messages,
			SourceWorktreePath: candidate.WorktreePath, SourceBranch: candidate.Branch,
			SourceRepo: candidate.SourceRepo,
		}
		result, forkErr := recovery.ForkConversation(
			request.Context(), continuation, body.Name, s.registry, adoptSourceFromSession(candidate),
		)
		if forkErr != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": forkErr.Error()}, corsOrigin)
			return
		}
		s.sendJSON(response, http.StatusCreated, result, corsOrigin)
	case request.URL.Path == "/api/recovery/adopt" && request.Method == http.MethodPost:
		var body struct {
			Target              string `json:"target"`
			HistoryID           string `json:"historyId,omitempty"`
			Name                string `json:"name,omitempty"`
			SourceSessionID     string `json:"sourceSessionId,omitempty"`
			RepairLaneID        string `json:"repairLaneId,omitempty"`
			DestinationProvider string `json:"destinationProvider,omitempty"`
			RuntimeMode         string `json:"runtimeMode,omitempty"`
			RemoteControl       *bool  `json:"remoteControl,omitempty"`
			Force               bool   `json:"force,omitempty"`
		}
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		if strings.TrimSpace(body.Target) == "" && strings.TrimSpace(body.HistoryID) == "" {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "target or historyId is required"}, corsOrigin)
			return
		}
		runtimeMode, runtimeModeErr := normalizeContinuationRuntime(body.RuntimeMode)
		if runtimeModeErr != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": runtimeModeErr.Error()}, corsOrigin)
			return
		}
		body.RuntimeMode = runtimeMode
		if body.RemoteControl != nil && *body.RemoteControl && body.RuntimeMode != "terminal" {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{
				"error": "Remote Control requires runtimeMode terminal",
			}, corsOrigin)
			return
		}
		recoveryMutationMu.Lock()
		defer recoveryMutationMu.Unlock()
		store, _, ok := s.openRecoveryReport(request.Context(), response, corsOrigin)
		if !ok {
			return
		}
		defer store.Close()
		var source *recovery.AdoptSource
		sourceCandidates := s.registry.List(true)
		sourceIndex := -1
		if body.SourceSessionID != "" {
			for index, candidate := range sourceCandidates {
				if candidate.ID != body.SourceSessionID {
					continue
				}
				sourceIndex = index
				break
			}
			if sourceIndex < 0 {
				s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "source session not found"}, corsOrigin)
				return
			}
		}
		resolveOptions := recovery.AdoptionOptions{}
		if sourceIndex >= 0 {
			candidate := sourceCandidates[sourceIndex]
			if candidate.ReopenedAs != "" && candidate.ReopenedAs != body.RepairLaneID {
				s.sendJSON(response, http.StatusConflict, map[string]any{
					"error":  "source session is already linked to successor " + candidate.ReopenedAs + "; no session was started",
					"laneId": candidate.ReopenedAs,
				}, corsOrigin)
				return
			}
			if candidate.ConfigDir != "" {
				if candidate.Tool == "claude-code" {
					resolveOptions.ClaudeProjectsDir = filepath.Join(candidate.ConfigDir, "projects")
				} else if candidate.Tool == "codex" {
					resolveOptions.CodexSessionsDir = filepath.Join(candidate.ConfigDir, "sessions")
				}
			}
		}
		var (
			adoption recovery.Adoption
			err      error
		)
		if strings.TrimSpace(body.HistoryID) != "" {
			history, historyErr := s.integrationEndpoints.LookupHistory(sourceCandidates, body.HistoryID)
			if historyErr != nil {
				s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "history conversation not found"}, corsOrigin)
				return
			}
			if history.ProviderSessionID == "" {
				s.sendJSON(response, http.StatusConflict, map[string]any{"error": "history conversation has no provider identity"}, corsOrigin)
				return
			}
			destination, destinationErr := normalizeContinuationProvider(body.DestinationProvider)
			if destinationErr != nil {
				s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": destinationErr.Error()}, corsOrigin)
				return
			}
			sourceProvider, sourceProviderErr := normalizeContinuationProvider(history.Tool)
			if sourceProviderErr != nil || sourceProvider == "" {
				s.sendJSON(response, http.StatusConflict, map[string]any{
					"error": "cross-provider continuation is available only for Claude and Codex histories",
				}, corsOrigin)
				return
			}
			if destination != "" && destination != sourceProvider {
				if body.RuntimeMode == "terminal" {
					s.sendJSON(response, http.StatusBadRequest, map[string]any{
						"error": "Terminal is available only when continuing with the original provider; cross-provider continuation uses Rich mode",
					}, corsOrigin)
					return
				}
				if body.RepairLaneID != "" {
					s.sendJSON(response, http.StatusBadRequest, map[string]any{
						"error": "cross-provider continuation repair is not available; the source remains untouched",
					}, corsOrigin)
					return
				}
				if history.PromptHistoryOnly || !history.ConversationAvailable {
					s.sendJSON(response, http.StatusConflict, map[string]any{
						"error": "cross-provider continuation requires the complete authored conversation, not only a provider prompt index",
					}, corsOrigin)
					return
				}
				if sourceIndex >= 0 {
					candidate := sourceCandidates[sourceIndex]
					providerUUID, _ := ledger.ExistingProviderResume(candidate.Cmd, candidate.Args)
					if providerUUID == "" {
						providerUUID = candidate.ConversationID
					}
					if providerUUID == "" {
						providerUUID = candidate.ClaudeSessionID
					}
					if providerUUID != history.ProviderSessionID {
						s.sendJSON(response, http.StatusConflict, map[string]any{
							"error": "source session belongs to a different provider conversation",
						}, corsOrigin)
						return
					}
					source = adoptSourceFromSession(candidate)
				}
				transcript, transcriptErr := s.integrationEndpoints.Transcript(sourceCandidates, history.ID)
				if transcriptErr != nil {
					s.sendJSON(response, http.StatusConflict, map[string]any{
						"error": "complete authored conversation is unavailable: " + transcriptErr.Error(),
					}, corsOrigin)
					return
				}
				messages := continuationMessages(transcript.Messages)
				mode := state.ContinuationLinkedSearch
				if destination == "codex" {
					mode = state.ContinuationNativeImport
				}
				continuation := state.ContinuationContext{
					SchemaVersion:   state.ContinuationSchemaVersion,
					SourceHistoryID: history.ID, SourceProvider: sourceProvider,
					SourceProviderID: history.ProviderSessionID, SourceTitle: history.Name,
					SourceCWD: history.CWD, DestinationProvider: destination,
					Mode: mode, Messages: messages,
				}
				if source != nil {
					continuation.SourceWorktreePath = source.WorktreePath
					continuation.SourceBranch = source.Branch
					continuation.SourceRepo = source.SourceRepo
				}
				result, continueErr := recovery.ContinueAcrossProviders(
					request.Context(), continuation, body.Name, s.registry, store.Observations(), source,
				)
				if continueErr != nil {
					s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": continueErr.Error()}, corsOrigin)
					return
				}
				status := http.StatusCreated
				if result.Partial {
					status = http.StatusAccepted
				}
				s.sendJSON(response, status, result, corsOrigin)
				return
			}
			if history.PromptHistoryOnly {
				adoption, err = recovery.ResolvePromptHistoryAdoption(
					history.ID, history.ProviderSessionID, history.CWD,
				)
			} else {
				adoption, err = recovery.ResolveAdoption(history.ProviderSessionID, resolveOptions)
				adoption.HistoryID = history.ID
			}
		} else {
			adoption, err = recovery.ResolveAdoption(body.Target, resolveOptions)
		}
		if err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		if sourceIndex >= 0 {
			candidate := sourceCandidates[sourceIndex]
			providerUUID, _ := ledger.ExistingProviderResume(candidate.Cmd, candidate.Args)
			if providerUUID == "" {
				providerUUID = candidate.ConversationID
			}
			if providerUUID == "" {
				providerUUID = candidate.ClaudeSessionID
			}
			if providerUUID != adoption.ProviderUUID {
				s.sendJSON(response, http.StatusConflict, map[string]any{
					"error": "source session belongs to a different provider conversation",
				}, corsOrigin)
				return
			}
			source = adoptSourceFromSession(candidate)
		}
		var claudeOptions *state.ClaudeSessionOptions
		if body.RemoteControl != nil {
			if adoption.Tool != string(state.ToolClaude) {
				s.sendJSON(response, http.StatusBadRequest, map[string]any{
					"error": "Remote Control is available only when continuing a Claude conversation in Terminal",
				}, corsOrigin)
				return
			}
			choice := state.ClaudeChoiceOff
			if *body.RemoteControl {
				choice = state.ClaudeChoiceOn
			}
			claudeOptions = &state.ClaudeSessionOptions{RemoteControl: choice}
		}
		if body.RepairLaneID != "" {
			successorSession, live := s.registry.Get(body.RepairLaneID)
			if !live {
				s.sendJSON(response, http.StatusConflict, map[string]any{
					"error":  "adoption repair requires the existing live successor; no session was started",
					"laneId": body.RepairLaneID,
				}, corsOrigin)
				return
			}
			successor := successorSession.Info()
			if successor.Exited {
				s.sendJSON(response, http.StatusConflict, map[string]any{
					"error":  "adoption repair requires the existing live successor; no session was started",
					"laneId": body.RepairLaneID,
				}, corsOrigin)
				return
			}
			providerUUID, _ := ledger.ExistingProviderResume(successor.Cmd, successor.Args)
			if providerUUID == "" {
				providerUUID = successor.ConversationID
			}
			if providerUUID == "" {
				providerUUID = successor.ClaudeSessionID
			}
			if providerUUID != adoption.ProviderUUID {
				s.sendJSON(response, http.StatusConflict, map[string]any{
					"error":  "adoption repair provider identity does not match the live successor; no session was started",
					"laneId": body.RepairLaneID,
				}, corsOrigin)
				return
			}
			result, err := recovery.RepairAdopt(
				request.Context(), adoption, body.Name, body.RepairLaneID, source,
				store, store.Boundaries(), store.Observations(),
			)
			if err != nil {
				s.sendJSON(response, http.StatusConflict, map[string]any{
					"error": err.Error(), "laneId": body.RepairLaneID,
				}, corsOrigin)
				return
			}
			status := http.StatusOK
			if result.Partial {
				status = http.StatusAccepted
			}
			s.sendJSON(response, status, result, corsOrigin)
			return
		}
		result, err := recovery.Adopt(
			request.Context(), adoption, body.Name, s.registry, store.Boundaries(), store.Observations(),
			recovery.AdoptOptions{
				Force: body.Force, Source: source, Events: store,
				RuntimeMode: body.RuntimeMode, Claude: claudeOptions,
			},
		)
		if err != nil {
			status := http.StatusBadRequest
			var live *sessionruntime.ConversationLiveError
			var moved *sessionruntime.ConversationMovedError
			if errors.As(err, &live) || errors.As(err, &moved) {
				status = http.StatusConflict
			}
			s.sendJSON(response, status, map[string]any{"error": err.Error(), "laneId": result.LaneID}, corsOrigin)
			return
		}
		status := http.StatusCreated
		if result.Partial {
			status = http.StatusAccepted
		}
		s.sendJSON(response, status, result, corsOrigin)
	default:
		s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "not found", "path": request.URL.Path}, corsOrigin)
	}
}

func normalizeContinuationProvider(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return "", nil
	case "claude", "claude-code":
		return "claude", nil
	case "codex":
		return "codex", nil
	default:
		return "", errors.New("destinationProvider must be claude or codex")
	}
}

func continuationMessages(messages []integrations.TranscriptMessage) []state.ContinuationMessage {
	result := make([]state.ContinuationMessage, 0, len(messages))
	for _, message := range messages {
		if message.Role != "user" && message.Role != "assistant" {
			continue
		}
		if strings.TrimSpace(message.Text) == "" {
			continue
		}
		timestamp := ""
		if message.Timestamp != nil {
			timestamp = *message.Timestamp
		}
		result = append(result, state.ContinuationMessage{
			Role: message.Role, Text: message.Text, Timestamp: timestamp,
		})
	}
	return result
}

func normalizeContinuationRuntime(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "rich":
		return strings.ToLower(strings.TrimSpace(value)), nil
	case "terminal":
		return "terminal", nil
	default:
		return "", errors.New("runtimeMode must be rich or terminal")
	}
}

func adoptSourceFromSession(candidate state.SessionInfo) *recovery.AdoptSource {
	displayParent := candidate.DisplayParentSessionID
	if displayParent == nil && candidate.ParentSessionID != "" {
		parent := candidate.ParentSessionID
		displayParent = &parent
	}
	return &recovery.AdoptSource{
		LaneID: candidate.ID, Name: candidate.Name, Description: candidate.Description,
		Tags: candidate.Tags, Profile: candidate.Profile, ConfigDir: candidate.ConfigDir, Kind: candidate.Kind,
		WorktreePath: candidate.WorktreePath, Branch: candidate.Branch, SourceRepo: candidate.SourceRepo,
		DisplayParentSessionID: displayParent,
	}
}

func (s *Server) openRecoveryReport(
	ctx context.Context,
	response http.ResponseWriter,
	corsOrigin string,
) (*ledger.Store, recovery.Report, bool) {
	store, err := ledger.Open(ctx, ledger.Options{})
	if err != nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return nil, recovery.Report{}, false
	}
	report, err := recovery.New(recovery.Options{
		Reader: store, RunnerStateDir: s.config.RunnerStateDir,
		ManagedSessions: s.registry.List(false),
	}).Report(ctx)
	if err != nil {
		_ = store.Close()
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return nil, recovery.Report{}, false
	}
	return store, report, true
}
