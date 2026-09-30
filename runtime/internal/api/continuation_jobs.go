package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

const (
	defaultContinuationTokenThreshold = 60_000
	maxContinuationTranscriptBytes    = 32 << 20
	continuationPollInterval          = 100 * time.Millisecond
	continuationFirstPrompt           = "Continue from this conversation. Briefly say what you understand the current task to be and what you would do next. Do not take any other action yet."
)

type continuationRequest struct {
	Target              string `json:"target"`
	HistoryID           string `json:"historyId,omitempty"`
	SourceSessionID     string `json:"sourceSessionId,omitempty"`
	DestinationProvider string `json:"destinationProvider"`
	Model               string `json:"model,omitempty"`
	Effort              string `json:"effort,omitempty"`
	MessageLimit        int    `json:"messageLimit,omitempty"`
	ConfirmWholeHistory bool   `json:"confirmWholeHistory,omitempty"`
}

type continuationPreview struct {
	Conversation        string `json:"conversation"`
	SourceProvider      string `json:"sourceProvider"`
	DestinationProvider string `json:"destinationProvider"`
	TotalMessageCount   int    `json:"totalMessageCount"`
	MessageCount        int    `json:"messageCount"`
	CharacterCount      int    `json:"characterCount"`
	EstimatedTokens     int    `json:"estimatedTokens"`
	ThresholdTokens     int    `json:"thresholdTokens"`
	Limited             bool   `json:"limited"`
	SourceUntouched     bool   `json:"sourceUntouched"`
}

type continuationJobEvent struct {
	Stage string `json:"stage"`
	Text  string `json:"text"`
	At    int64  `json:"at"`
}

type continuationJobView struct {
	ID               string                 `json:"id"`
	Status           string                 `json:"status"`
	Stage            string                 `json:"stage"`
	StageText        string                 `json:"stageText"`
	Provider         string                 `json:"provider"`
	Model            string                 `json:"model,omitempty"`
	ModelDisplayName string                 `json:"modelDisplayName,omitempty"`
	Effort           string                 `json:"effort,omitempty"`
	LaneID           string                 `json:"laneId,omitempty"`
	Preview          *continuationPreview   `json:"preview,omitempty"`
	Events           []continuationJobEvent `json:"events"`
	Error            string                 `json:"error,omitempty"`
	Warning          string                 `json:"warning,omitempty"`
	FailureKind      string                 `json:"failureKind,omitempty"`
	FailureDetail    string                 `json:"failureDetail,omitempty"`
	Retry            *proto.ProviderRetry   `json:"retry,omitempty"`
}

type continuationJob struct {
	mu     sync.Mutex
	view   continuationJobView
	ctx    context.Context
	cancel context.CancelFunc
}

type continuationJobStore struct {
	mu   sync.Mutex
	jobs map[string]*continuationJob
}

type preparedContinuation struct {
	context  state.ContinuationContext
	preview  continuationPreview
	history  string
	sourceID string
}

func newContinuationJobStore() *continuationJobStore {
	return &continuationJobStore{jobs: make(map[string]*continuationJob)}
}

func (s *Server) handleContinuationRoute(
	response http.ResponseWriter, request *http.Request, corsOrigin string,
) bool {
	const root = "/api/recovery/continuation"
	if !strings.HasPrefix(request.URL.Path, root) {
		return false
	}
	switch {
	case request.URL.Path == root+"/preview" && request.Method == http.MethodPost:
		s.handleContinuationPreview(response, request, corsOrigin)
	case request.URL.Path == root+"/jobs" && request.Method == http.MethodPost:
		s.handleContinuationStart(response, request, corsOrigin)
	case strings.HasPrefix(request.URL.Path, root+"/jobs/"):
		s.handleContinuationJob(response, request, corsOrigin, strings.TrimPrefix(request.URL.Path, root+"/jobs/"))
	default:
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
	}
	return true
}

func (s *Server) handleContinuationPreview(
	response http.ResponseWriter, request *http.Request, corsOrigin string,
) {
	var body continuationRequest
	if err := readJSON(request, &body); err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	prepared, err := s.prepareContinuation(request.Context(), body)
	if err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	s.sendJSON(response, http.StatusOK, prepared.preview, corsOrigin)
}

func (s *Server) handleContinuationStart(
	response http.ResponseWriter, request *http.Request, corsOrigin string,
) {
	var body continuationRequest
	if err := readJSON(request, &body); err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	if strings.TrimSpace(body.HistoryID) == "" && strings.TrimSpace(body.Target) == "" {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "target or historyId is required"}, corsOrigin)
		return
	}
	job, err := s.continuationJobs.create(body.DestinationProvider)
	if err != nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	job.stage("exporting-history", "Exporting conversation history")
	go s.runContinuationJob(job, body)
	s.sendJSON(response, http.StatusAccepted, job.snapshot(), corsOrigin)
}

func (s *Server) handleContinuationJob(
	response http.ResponseWriter, request *http.Request, corsOrigin, id string,
) {
	job := s.continuationJobs.get(id)
	if job == nil {
		s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "continuation job not found"}, corsOrigin)
		return
	}
	switch request.Method {
	case http.MethodGet:
		s.sendJSON(response, http.StatusOK, job.snapshot(), corsOrigin)
	case http.MethodDelete:
		s.cancelContinuationJob(job)
		s.sendJSON(response, http.StatusOK, job.snapshot(), corsOrigin)
	default:
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
	}
}

func (store *continuationJobStore) create(provider string) (*continuationJob, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return nil, fmt.Errorf("create continuation job id: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &continuationJob{
		ctx: ctx, cancel: cancel,
		view: continuationJobView{
			ID: hex.EncodeToString(bytes), Status: "running", Provider: strings.ToLower(strings.TrimSpace(provider)),
			Events: make([]continuationJobEvent, 0, 5),
		},
	}
	store.mu.Lock()
	store.jobs[job.view.ID] = job
	store.mu.Unlock()
	return job, nil
}

func (store *continuationJobStore) get(id string) *continuationJob {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.jobs[id]
}

func (job *continuationJob) snapshot() continuationJobView {
	job.mu.Lock()
	defer job.mu.Unlock()
	result := job.view
	result.Events = append([]continuationJobEvent(nil), job.view.Events...)
	return result
}

func (job *continuationJob) stage(stage, text string) {
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.view.Status != "running" {
		return
	}
	job.view.Stage = stage
	job.view.StageText = text
	job.view.Events = append(job.view.Events, continuationJobEvent{Stage: stage, Text: text, At: time.Now().UnixMilli()})
}

func (job *continuationJob) configure(
	preview continuationPreview, model, displayName, effort string,
) {
	job.mu.Lock()
	defer job.mu.Unlock()
	job.view.Preview = &preview
	job.view.Model = model
	job.view.ModelDisplayName = displayName
	job.view.Effort = effort
}

func (job *continuationJob) created(laneID string) {
	job.mu.Lock()
	job.view.LaneID = laneID
	job.mu.Unlock()
}

func (job *continuationJob) providerFault(info state.SessionInfo) {
	job.mu.Lock()
	job.view.FailureKind = info.FailureKind
	job.view.FailureDetail = info.FailureDetail
	job.view.Retry = info.Retry
	job.mu.Unlock()
}

func (job *continuationJob) finish(status, text, errText string) {
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.view.Status != "running" && status != "canceled" {
		return
	}
	job.view.Status = status
	job.view.StageText = text
	job.view.Error = errText
	job.view.FailureKind = ""
	job.view.FailureDetail = ""
	job.view.Retry = nil
}

func (s *Server) runContinuationJob(job *continuationJob, body continuationRequest) {
	prepared, err := s.prepareContinuation(job.ctx, body)
	if err != nil {
		s.finishContinuationError(job, err)
		return
	}
	if prepared.preview.EstimatedTokens > prepared.preview.ThresholdTokens &&
		!prepared.preview.Limited && !body.ConfirmWholeHistory {
		s.finishContinuationError(job, errors.New("confirmWholeHistory is required above the token threshold"))
		return
	}
	model, displayName, effort, err := s.resolveContinuationModel(job.ctx, prepared.context.DestinationProvider, body.Model, body.Effort)
	if err != nil {
		s.finishContinuationError(job, err)
		return
	}
	prepared.context.DestinationModel = model
	prepared.context.DestinationModelName = displayName
	prepared.context.DestinationEffort = effort
	job.configure(prepared.preview, model, displayName, effort)
	if job.ctx.Err() != nil {
		return
	}
	job.stage("creating-session", "Creating the new "+providerDisplayName(prepared.context.DestinationProvider)+" session")
	result, err := s.createContinuationSession(job.ctx, prepared)
	if err != nil {
		s.finishContinuationError(job, err)
		return
	}
	job.created(result.LaneID)
	job.stage("provider-starting", providerDisplayName(prepared.context.DestinationProvider)+" is starting")
	sentAt := time.Now().UnixMilli()
	if !s.registry.Input(job.ctx, result.LaneID, continuationFirstPrompt) {
		s.finishContinuationError(job, errors.New("the new session did not accept its first message"))
		return
	}
	job.stage("first-reply", "Waiting for "+providerDisplayName(prepared.context.DestinationProvider)+" to answer")
	s.waitForContinuationReply(job, prepared.sourceID, sentAt)
}

func (s *Server) createContinuationSession(
	ctx context.Context, prepared preparedContinuation,
) (recovery.AdoptResult, error) {
	recoveryMutationMu.Lock()
	defer recoveryMutationMu.Unlock()
	source, err := s.continuationSource(prepared.sourceID, prepared.history)
	if err != nil {
		return recovery.AdoptResult{}, err
	}
	createSource := source
	if source != nil {
		copy := *source
		copy.LaneID = ""
		createSource = &copy
	}
	return recovery.ContinueAcrossProviders(
		ctx, prepared.context, "", s.registry, nil, createSource,
	)
}

func (s *Server) waitForContinuationReply(job *continuationJob, sourceID string, sentAt int64) {
	ticker := time.NewTicker(continuationPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-job.ctx.Done():
			return
		case <-ticker.C:
			view := job.snapshot()
			session, ok := s.registry.Get(view.LaneID)
			if !ok {
				s.finishContinuationError(job, errors.New("the new session is no longer available"))
				return
			}
			info := session.Info()
			if info.FailureKind != "" {
				job.providerFault(info)
			}
			if info.Exited {
				s.finishContinuationError(job, errors.New("the new session ended before the agent answered"))
				return
			}
			if info.IdleReason == state.IdleReasonCompleted && info.IdleSince != nil && *info.IdleSince >= sentAt {
				warning := s.recordContinuationSuccess(sourceID, view.LaneID)
				job.mu.Lock()
				job.view.Warning = warning
				job.mu.Unlock()
				job.finish("succeeded", providerDisplayName(view.Provider)+" answered", "")
				return
			}
		}
	}
}

func (s *Server) cancelContinuationJob(job *continuationJob) {
	view := job.snapshot()
	if view.Status != "running" {
		return
	}
	job.cancel()
	if view.LaneID != "" {
		if err := s.registry.RequestKill(context.Background(), view.LaneID, false); err != nil {
			job.finish("failed", "Sessions could not end the new session", err.Error())
			return
		}
		if !s.waitForSessionEnded(view.LaneID, 5*time.Second) {
			job.finish("failed", "Sessions could not confirm that the new session ended", "Check the new session before trying again.")
			return
		}
		job.finish("canceled", "The new "+providerDisplayName(view.Provider)+" session was ended. The original conversation was not changed.", "")
		return
	}
	job.finish("canceled", "Canceled before a new session was created. The original conversation was not changed.", "")
}

func (s *Server) waitForSessionEnded(id string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		session, ok := s.registry.Get(id)
		if ok && session.Info().Exited {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func (s *Server) finishContinuationError(job *continuationJob, err error) {
	if errors.Is(err, context.Canceled) || job.ctx.Err() != nil {
		return
	}
	job.finish("failed", "The continuation could not start", err.Error())
}

func (s *Server) prepareContinuation(
	ctx context.Context, body continuationRequest,
) (preparedContinuation, error) {
	if body.MessageLimit < 0 {
		return preparedContinuation{}, errors.New("messageLimit must be zero or greater")
	}
	historyID := strings.TrimSpace(body.HistoryID)
	if historyID == "" {
		historyID = strings.TrimSpace(body.Target)
	}
	candidates := s.registry.List(true)
	history, err := s.integrationEndpoints.LookupHistory(candidates, historyID)
	if err != nil {
		return preparedContinuation{}, errors.New("conversation history was not found")
	}
	sourceProvider, err := normalizeContinuationProvider(history.Tool)
	if err != nil || sourceProvider == "" {
		return preparedContinuation{}, errors.New("only Claude and Codex conversations can continue with another agent")
	}
	destination, err := normalizeContinuationProvider(body.DestinationProvider)
	if err != nil || destination == "" {
		return preparedContinuation{}, errors.New("choose Claude or Codex for the new conversation")
	}
	if destination == sourceProvider {
		return preparedContinuation{}, errors.New("the continuation job is only for changing agents")
	}
	if history.PromptHistoryOnly || !history.ConversationAvailable {
		return preparedContinuation{}, errors.New("the complete conversation is not available to continue with another agent")
	}
	transcript, err := s.integrationEndpoints.TranscriptLimitedContext(
		ctx, candidates, history.ID, maxContinuationTranscriptBytes,
	)
	if err != nil {
		return preparedContinuation{}, fmt.Errorf("read the conversation: %w", err)
	}
	allMessages := continuationMessages(transcript.Messages)
	messages := limitContinuationMessages(allMessages, body.MessageLimit)
	if len(messages) == 0 {
		return preparedContinuation{}, errors.New("the conversation has no user or agent messages to continue")
	}
	continuation := state.ContinuationContext{
		SchemaVersion: state.ContinuationSchemaVersion, SourceHistoryID: history.ID,
		SourceProvider: sourceProvider, SourceProviderID: history.ProviderSessionID,
		SourceTitle: history.Name, SourceCWD: history.CWD, DestinationProvider: destination,
		Mode: state.ContinuationLinkedSearch, Messages: messages,
	}
	if destination == "codex" {
		continuation.Mode = state.ContinuationNativeImport
	}
	sourceID := continuationSourceID(candidates, history.ID, body.SourceSessionID)
	preview := previewContinuation(continuation, len(allMessages))
	return preparedContinuation{context: continuation, preview: preview, history: history.ProviderSessionID, sourceID: sourceID}, nil
}

func limitContinuationMessages(messages []state.ContinuationMessage, limit int) []state.ContinuationMessage {
	if limit <= 0 || limit >= len(messages) {
		return messages
	}
	return messages[len(messages)-limit:]
}

func previewContinuation(continuation state.ContinuationContext, total int) continuationPreview {
	characters := 0
	for _, message := range continuation.Messages {
		characters += utf8.RuneCountInString(message.Text)
	}
	return continuationPreview{
		Conversation: continuation.SourceTitle, SourceProvider: continuation.SourceProvider,
		DestinationProvider: continuation.DestinationProvider,
		TotalMessageCount:   total, MessageCount: len(continuation.Messages),
		CharacterCount: characters, EstimatedTokens: (characters + 3) / 4,
		ThresholdTokens: continuationTokenThreshold(), Limited: len(continuation.Messages) < total,
		SourceUntouched: true,
	}
}

func continuationTokenThreshold() int {
	raw := strings.TrimSpace(os.Getenv("SESSIONS_CONTINUATION_TOKEN_THRESHOLD"))
	if raw == "" {
		return defaultContinuationTokenThreshold
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return defaultContinuationTokenThreshold
	}
	return value
}

func continuationSourceID(
	candidates []state.SessionInfo, historyID, requested string,
) string {
	if requested = strings.TrimSpace(requested); requested != "" {
		return requested
	}
	for _, candidate := range candidates {
		if candidate.ID == historyID {
			return candidate.ID
		}
	}
	return ""
}

func (s *Server) continuationSource(sourceID, providerID string) (*recovery.AdoptSource, error) {
	if strings.TrimSpace(sourceID) == "" {
		return nil, nil
	}
	for _, candidate := range s.registry.List(true) {
		if candidate.ID != sourceID {
			continue
		}
		if candidate.ReopenedAs != "" {
			return nil, errors.New("the source conversation already has a successor")
		}
		identity, _ := ledger.ExistingProviderResume(candidate.Cmd, candidate.Args)
		if identity == "" {
			identity = candidate.ConversationID
		}
		if identity == "" {
			identity = candidate.ClaudeSessionID
		}
		if providerID != "" && identity != "" && identity != providerID {
			return nil, errors.New("the selected Sessions row belongs to a different conversation")
		}
		return adoptSourceFromSession(candidate), nil
	}
	return nil, errors.New("source session not found")
}

func (s *Server) resolveContinuationModel(
	ctx context.Context, provider, requestedModel, requestedEffort string,
) (string, string, string, error) {
	models, err := s.continuationModels(ctx, provider)
	if err != nil {
		return "", "", "", err
	}
	requestedModel = strings.TrimSpace(requestedModel)
	var selected *codexapp.Model
	for index := range models {
		if models[index].ID == requestedModel || (requestedModel == "" && models[index].IsDefault) {
			selected = &models[index]
			break
		}
	}
	if selected == nil {
		return "", "", "", fmt.Errorf("model %q is not available for %s", requestedModel, providerDisplayName(provider))
	}
	effort := strings.TrimSpace(requestedEffort)
	if effort == "" {
		effort = selected.DefaultReasoningEffort
	}
	if effort != "" && !supportsContinuationEffort(*selected, effort) {
		return "", "", "", fmt.Errorf("effort %q is not available for model %s", effort, selected.DisplayName)
	}
	name := strings.TrimSpace(selected.DisplayName)
	if name == "" {
		name = selected.ID
	}
	return selected.ID, name, effort, nil
}

func (s *Server) continuationModels(ctx context.Context, provider string) ([]codexapp.Model, error) {
	if provider == "claude" {
		settings, err := s.loadClaudeSettings()
		if err != nil {
			return nil, err
		}
		return claudeModelOptions(settings.Model, settings.Effort), nil
	}
	catalog, ok := s.registry.(newSessionModelCatalogService)
	if !ok {
		return nil, errors.New("Codex model choices are not available on this runtime")
	}
	return catalog.CodexModelOptions(ctx)
}

func supportsContinuationEffort(model codexapp.Model, effort string) bool {
	for _, option := range model.SupportedReasoningEfforts {
		if option.ReasoningEffort == effort {
			return true
		}
	}
	return false
}

func (s *Server) recordContinuationSuccess(sourceID, laneID string) string {
	if sourceID == "" {
		return ""
	}
	store, err := ledger.Open(context.Background(), ledger.Options{})
	if err != nil {
		return "The new conversation is ready, but Sessions could not record where it came from: " + err.Error()
	}
	defer store.Close()
	err = store.Observations().RecordReopened(context.Background(), ledger.Reopened{
		Meta: ledger.Meta{LaneID: sourceID}, NewLaneID: laneID,
	})
	if err != nil {
		return "The new conversation is ready, but Sessions could not record where it came from: " + err.Error()
	}
	s.clearPausedRestore(sourceID)
	return ""
}

func providerDisplayName(provider string) string {
	if provider == "codex" {
		return "Codex"
	}
	return "Claude"
}
