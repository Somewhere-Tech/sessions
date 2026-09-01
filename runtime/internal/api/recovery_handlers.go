package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
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
		for _, outcome := range result.Outcomes {
			if outcome.NewLaneID != "" {
				s.clearPausedRestore(outcome.SourceLaneID)
			}
		}
		s.sendJSON(response, http.StatusOK, result, corsOrigin)
	case request.URL.Path == "/api/recovery/fork" && request.Method == http.MethodPost:
		var body struct {
			SourceSessionID     string `json:"sourceSessionId"`
			DestinationProvider string `json:"destinationProvider,omitempty"`
			Name                string `json:"name,omitempty"`
			SourceMessageIndex  *int   `json:"sourceMessageIndex,omitempty"`
			SourceMessageID     string `json:"sourceMessageId,omitempty"`
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
		sourceCandidates := s.registry.List(true)
		sourceSession, live := s.registry.Get(body.SourceSessionID)
		var candidate state.SessionInfo
		if live {
			candidate = sourceSession.Info()
		} else {
			for _, item := range sourceCandidates {
				if item.ID == body.SourceSessionID {
					candidate = item
					break
				}
			}
		}
		if candidate.ID == "" {
			s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "source session not found"}, corsOrigin)
			return
		}
		if live && candidate.Working {
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
		selectedMessages, pointIndex, pointID, pointErr := forkTranscriptMessages(
			transcript.Messages, body.SourceMessageIndex, body.SourceMessageID,
		)
		if pointErr != nil {
			s.sendJSON(response, http.StatusConflict, map[string]any{
				"error": pointErr.Error(),
			}, corsOrigin)
			return
		}
		messages := continuationMessages(selectedMessages)
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
			ForkPointIndex: pointIndex, ForkPointMessageID: pointID,
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
			Target               string `json:"target"`
			HistoryID            string `json:"historyId,omitempty"`
			Name                 string `json:"name,omitempty"`
			SourceSessionID      string `json:"sourceSessionId,omitempty"`
			RepairLaneID         string `json:"repairLaneId,omitempty"`
			DestinationProvider  string `json:"destinationProvider,omitempty"`
			RuntimeMode          string `json:"runtimeMode,omitempty"`
			RemoteControl        *bool  `json:"remoteControl,omitempty"`
			ClaudePermissionMode string `json:"claudePermissionMode,omitempty"`
			Force                bool   `json:"force,omitempty"`
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
			adoption                recovery.Adoption
			err                     error
			resolvedHistoryID       string
			resolvedHistoryProvider string
		)
		if strings.TrimSpace(body.HistoryID) != "" {
			history, historyErr := s.integrationEndpoints.LookupHistory(sourceCandidates, body.HistoryID)
			if historyErr != nil {
				s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "history conversation not found"}, corsOrigin)
				return
			}
			resolvedHistoryID = history.ID
			resolvedHistoryProvider = history.ProviderSessionID
			// Older runners can have a complete, Sessions-owned history record but
			// no copied provider handle. Prefer the provider identity recovered by
			// History from session_meta. If it is genuinely absent, the authored
			// transcript is still enough to create one linked successor.
			if sourceIndex < 0 {
				for index, candidate := range sourceCandidates {
					if candidate.ID == history.ID {
						sourceIndex = index
						break
					}
				}
			}
			if sourceIndex >= 0 && source == nil {
				candidate := sourceCandidates[sourceIndex]
				if candidate.ReopenedAs != "" && candidate.ReopenedAs != body.RepairLaneID {
					s.sendJSON(response, http.StatusConflict, map[string]any{
						"error":  "source session is already linked to successor " + candidate.ReopenedAs + "; no session was started",
						"laneId": candidate.ReopenedAs,
					}, corsOrigin)
					return
				}
				source = adoptSourceFromSession(candidate)
			}
			// A title/history-id resume can discover its source record only after
			// History resolves it. Carry that record's private provider root into
			// native identity resolution just as an explicit --source would.
			if sourceIndex >= 0 {
				candidate := sourceCandidates[sourceIndex]
				if candidate.ConfigDir != "" {
					if candidate.Tool == "claude-code" {
						resolveOptions.ClaudeProjectsDir = filepath.Join(candidate.ConfigDir, "projects")
					} else if candidate.Tool == "codex" {
						resolveOptions.CodexSessionsDir = filepath.Join(candidate.ConfigDir, "sessions")
					}
				}
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
			// A missing provider handle is not the only way a native resume
			// becomes impossible. The handle can be perfectly well known while
			// the provider's own transcript is gone -- Claude prunes on a
			// timer -- and Sessions answers from the copy it kept. Handing the
			// provider a resume flag for a file it no longer has is refused,
			// so those conversations take the same transcript path as one
			// whose handle was never recorded. Without this the recovery plan
			// correctly labels them transcript-recovery and the resume route
			// then fails with "no conversation source exists".
			restoreFromTranscript := history.ProviderSessionID == ""
			if !restoreFromTranscript {
				source, sourceErr := s.integrationEndpoints.Source(sourceCandidates, history.ID)
				restoreFromTranscript = sourceErr == nil &&
					source.SourceKind == string(watch.ClaudeMirror)
			}
			if restoreFromTranscript {
				if strings.TrimSpace(body.ClaudePermissionMode) != "" {
					s.sendJSON(response, http.StatusConflict, map[string]any{
						"error": "A permission override requires Claude's native resume handle; this conversation can only be restored from its Sessions transcript.",
					}, corsOrigin)
					return
				}
				if history.PromptHistoryOnly || !history.ConversationAvailable {
					s.sendJSON(response, http.StatusConflict, map[string]any{
						"error": "This conversation has neither a provider resume handle nor a complete Sessions transcript.",
					}, corsOrigin)
					return
				}
				if body.RuntimeMode == "terminal" {
					s.sendJSON(response, http.StatusBadRequest, map[string]any{
						"error": "The provider resume handle is missing, so Sessions must restore the conversation from its authored transcript in Conversation view; Terminal cannot import that history.",
					}, corsOrigin)
					return
				}
				if destination == "" {
					destination = sourceProvider
				}
				transcript, transcriptErr := s.integrationEndpoints.Transcript(sourceCandidates, history.ID)
				if transcriptErr != nil {
					s.sendJSON(response, http.StatusConflict, map[string]any{
						"error": "Sessions found the conversation record but could not read its authored history: " + transcriptErr.Error(),
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
					SourceTitle: history.Name, SourceCWD: history.CWD,
					DestinationProvider: destination, Mode: mode, Messages: messages,
				}
				if source != nil {
					continuation.SourceWorktreePath = source.WorktreePath
					continuation.SourceBranch = source.Branch
					continuation.SourceRepo = source.SourceRepo
				}
				var result recovery.AdoptResult
				var continueErr error
				if destination == sourceProvider {
					result, continueErr = recovery.ResumeFromTranscript(
						request.Context(), continuation, body.Name, s.registry,
						store.Observations(), source,
					)
				} else {
					result, continueErr = recovery.ContinueAcrossProviders(
						request.Context(), continuation, body.Name, s.registry,
						store.Observations(), source,
					)
				}
				if continueErr != nil {
					s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": continueErr.Error()}, corsOrigin)
					return
				}
				status := http.StatusCreated
				if result.Partial {
					status = http.StatusAccepted
				}
				if source != nil && result.LaneID != "" {
					s.clearPausedRestore(source.LaneID)
				}
				s.sendJSON(response, status, result, corsOrigin)
				return
			}
			if destination != "" && destination != sourceProvider {
				if strings.TrimSpace(body.ClaudePermissionMode) != "" {
					s.sendJSON(response, http.StatusBadRequest, map[string]any{
						"error": "--permissions applies only to a same-provider Claude resume, not a cross-provider copy",
					}, corsOrigin)
					return
				}
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
					if providerUUID == "" && candidate.ID == history.ID {
						// History resolved this exact managed row to the provider's
						// authoritative source file. Older rows may lack the copied
						// UUID even though session_meta contains it.
						providerUUID = history.ProviderSessionID
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
				if source != nil && result.LaneID != "" {
					s.clearPausedRestore(source.LaneID)
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
			if providerUUID == "" && candidate.ID == resolvedHistoryID {
				providerUUID = resolvedHistoryProvider
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
		if body.RemoteControl != nil || strings.TrimSpace(body.ClaudePermissionMode) != "" {
			if adoption.Tool != string(state.ToolClaude) {
				s.sendJSON(response, http.StatusBadRequest, map[string]any{
					"error": "Claude runtime options are available only when continuing a Claude conversation",
				}, corsOrigin)
				return
			}
			claudeOptions = &state.ClaudeSessionOptions{}
			if body.RemoteControl != nil {
				choice := state.ClaudeChoiceOff
				if *body.RemoteControl {
					choice = state.ClaudeChoiceOn
				}
				claudeOptions.RemoteControl = choice
			}
			if mode := strings.TrimSpace(body.ClaudePermissionMode); mode != "" {
				claudeOptions.PermissionMode = mode
				if _, normalizeErr := state.ResolveClaudeSettings(state.DefaultClaudeSettings(), claudeOptions); normalizeErr != nil {
					s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": normalizeErr.Error()}, corsOrigin)
					return
				}
			}
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
			if source != nil && result.LaneID != "" {
				s.clearPausedRestore(source.LaneID)
			}
			s.sendJSON(response, status, result, corsOrigin)
			return
		}
		result, err := recovery.Adopt(
			request.Context(), adoption, body.Name, s.registry, store.Boundaries(), store.Observations(),
			recovery.AdoptOptions{
				Force: body.Force, Source: source, Events: store,
				RuntimeMode: body.RuntimeMode, Claude: claudeOptions,
				ClaudeLive: claudeLiveQuery(sourceCandidates, resolveOptions.ClaudeProjectsDir),
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
		if source != nil && result.LaneID != "" {
			s.clearPausedRestore(source.LaneID)
		}
		s.sendJSON(response, status, result, corsOrigin)
	default:
		s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "not found", "path": request.URL.Path}, corsOrigin)
	}
}

func (s *Server) clearPausedRestore(sourceSessionID string) {
	if sourceSessionID == "" {
		return
	}
	_ = os.Remove(state.For(s.config.RunnerStateDir, sourceSessionID).RestorePending)
}

func forkTranscriptMessages(
	messages []integrations.TranscriptMessage,
	requestedIndex *int,
	requestedMessageID string,
) ([]integrations.TranscriptMessage, *int, string, error) {
	if requestedIndex == nil {
		if strings.TrimSpace(requestedMessageID) != "" {
			return nil, nil, "", errors.New("sourceMessageId requires sourceMessageIndex")
		}
		return messages, nil, "", nil
	}
	if *requestedIndex < 0 {
		return nil, nil, "", errors.New("sourceMessageIndex must be non-negative")
	}
	for position, message := range messages {
		if message.Index != *requestedIndex {
			continue
		}
		if message.Role != "user" && message.Role != "assistant" {
			return nil, nil, "", errors.New("fork point must be a user or agent message")
		}
		if expected := strings.TrimSpace(requestedMessageID); expected != "" && message.ID != expected {
			return nil, nil, "", errors.New("the selected message changed; reload the conversation before forking")
		}
		point := *requestedIndex
		return messages[:position+1], &point, message.ID, nil
	}
	return nil, nil, "", errors.New("the selected fork point is no longer available")
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

// claudeLiveQuery configures the read that answers "does another live Claude
// process already have this conversation open". Adoption reports the answer and
// proceeds either way, so the only thing this has to get right is which
// processes count as Sessions' own -- a wrong answer here is a false accusation
// against the user's own fleet, or silence about a genuine second window.
//
// claudeProjectsDir, when the source record named a profile, is that profile's
// CLAUDE_CONFIG_DIR/projects. Claude writes its live registry beside that
// projects tree under the same root, so a profile conversation has to be looked
// up in the profile's registry; reading the default ~/.claude one would examine
// a completely different set of processes.
func claudeLiveQuery(candidates []state.SessionInfo, claudeProjectsDir string) *watch.ClaudeLiveQuery {
	query := &watch.ClaudeLiveQuery{OwnedPIDs: ownedRunnerPIDs(candidates)}
	if trimmed := strings.TrimSpace(claudeProjectsDir); trimmed != "" {
		query.Dir = filepath.Join(filepath.Dir(trimmed), watch.ClaudeLiveRegistryDirName)
	}
	return query
}

// ownedRunnerPIDs is every process the manager currently has a session running
// as. It is the ownership seed for the live-registry read.
//
// It has to come from the manager's list, not from the daemon's own process
// tree. Sessions starts its runners through launchd, so a runner -- and the
// provider process under it -- is not a descendant of the daemon. Verified
// against a live machine: Claude pid 22440 has parent sessions-runner 22425,
// whose parent is launchd (pid 1), while the daemon is pid 91118 and appears
// nowhere in that chain. Seeding ownership with os.Getpid() therefore resolves
// nothing, and every conversation Sessions itself is running would be reported
// as somebody else's window. The manager's row for that same session carries
// pid 22440 directly, and ancestry in the registry read covers the case where
// the row carries the runner pid and Claude is its child.
func ownedRunnerPIDs(candidates []state.SessionInfo) []int {
	pids := make([]int, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Exited || candidate.PID <= 0 {
			continue
		}
		pids = append(pids, candidate.PID)
	}
	return pids
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
