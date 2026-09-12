package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

const (
	transcriptPreviewMaxBytes    = 2 * 1024 * 1024
	transcriptPreviewMaxMessages = 400
)

func (s *Server) handleIntegrationsRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	path := request.URL.Path
	matched := path == "/api/history" || strings.HasPrefix(path, "/api/history/") || path == "/api/errors"
	if !matched {
		return false
	}
	if request.Method != http.MethodGet {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return true
	}
	timer := newStageTimer()
	// One live listing per request. This used to be three — the tracking loop,
	// the failure observation, and the listing itself each asked the registry
	// again — and every one of them re-folds the ledger and re-probes every
	// runner. On a cold daemon that was most of the twelve seconds.
	live := s.liveSessions(timer)
	s.refreshIntegrations(live, timer)

	switch {
	case path == "/api/history":
		s.sendHistoryListing(response, request, corsOrigin, live, timer)
		return true
	case path == "/api/errors":
		since, err := errorsSince(request)
		if err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		feed, err := s.integrationEndpoints.ErrorFeed(since)
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, feed, corsOrigin)
		return true
	}

	id, variant, ok := historyPath(path)
	if !ok {
		s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "not found", "path": path}, corsOrigin)
		return true
	}
	if variant == "raw" {
		encoded, err := s.integrationEndpoints.Raw(s.registry.List(true), id)
		if errors.Is(err, integrations.ErrHistoryNotFound) {
			s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "history session not found", "id": id}, corsOrigin)
			return true
		}
		if err != nil {
			s.integrationError(response, corsOrigin, "raw history read failed", err)
			return true
		}
		s.sendIntegrationBytes(response, http.StatusOK, "application/octet-stream", encoded, corsOrigin)
		return true
	}
	if variant == "source" {
		source, err := s.integrationEndpoints.Source(s.registry.List(true), id)
		if errors.Is(err, integrations.ErrHistoryNotFound) {
			s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "history session not found", "id": id}, corsOrigin)
			return true
		}
		if err != nil {
			s.integrationError(response, corsOrigin, "history source lookup failed", err)
			return true
		}
		s.sendJSON(response, http.StatusOK, source, corsOrigin)
		return true
	}

	format := request.URL.Query().Get("format")
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "text" {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "format must be json or text"}, corsOrigin)
		return true
	}
	var transcript integrations.TranscriptResponse
	var err error
	switch variant {
	case "preview":
		maxMessages := transcriptPreviewMaxMessages
		if raw := request.URL.Query().Get("limit"); raw != "" {
			requested, parseErr := strconv.Atoi(raw)
			if parseErr != nil || requested < 1 || requested > transcriptPreviewMaxMessages {
				s.sendJSON(response, http.StatusBadRequest, map[string]any{
					"error": fmt.Sprintf("preview limit must be between 1 and %d", transcriptPreviewMaxMessages),
				}, corsOrigin)
				return true
			}
			maxMessages = requested
		}
		transcript, err = s.integrationEndpoints.TranscriptPreview(
			s.registry.List(true), id, transcriptPreviewMaxBytes, maxMessages,
		)
	case "window":
		var options integrations.TranscriptWindowOptions
		options, err = transcriptWindowOptions(request)
		if err == nil {
			transcript, err = s.integrationEndpoints.TranscriptWindow(s.registry.List(true), id, options)
		}
	default:
		transcript, err = s.integrationEndpoints.Transcript(s.registry.List(true), id)
	}
	if err != nil && variant == "window" {
		var queryError *historyQueryError
		if errors.As(err, &queryError) {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
	}
	if errors.Is(err, integrations.ErrHistoryNotFound) {
		s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "history session not found", "id": id}, corsOrigin)
		return true
	}
	if errors.Is(err, integrations.ErrHistoryChanged) {
		s.sendJSON(response, http.StatusConflict, map[string]any{
			"error": "This conversation changed after the search result was created. Run the search again to refresh its bookmark.",
			"id":    id,
		}, corsOrigin)
		return true
	}
	if err != nil {
		s.integrationError(response, corsOrigin, "history transcript failed", err)
		return true
	}
	if err := s.annotateTranscript(request.Context(), &transcript); err != nil {
		log.Printf("[attribution] annotate transcript for %s: %v", transcript.Session.ID, err)
	}
	if format == "text" {
		response.Header().Set("X-Sessions-Schema-Version", strconv.Itoa(integrations.SchemaVersion))
		s.sendIntegrationBytes(response, http.StatusOK, "text/plain; charset=utf-8", []byte(formatTranscriptText(transcript)), corsOrigin)
		return true
	}
	s.sendJSON(response, http.StatusOK, transcript, corsOrigin)
	return true
}

// historyListResponse is the body both /api/history views return. It is the
// store's own response plus one honesty field, so a client can read either view
// with the same decoder.
type historyListResponse struct {
	integrations.HistoryResponse
	// TranscriptsUnread marks a listing that never opened the transcripts it
	// listed. `summary=true` resolves and stats each source but deliberately
	// does not parse it, so on that view `skipped_records` is unknown rather
	// than zero and `unreadable` reports only what a stat could see. The field
	// is omitted when every listed transcript was read, which keeps the
	// documented rule that an absent counter means nothing was lost — without
	// it, the cheap view (the one a UI or agent polls) would report a torn
	// history exactly like a clean one.
	TranscriptsUnread bool `json:"transcripts_unread,omitempty"`
	// Timing is where this listing spent its time, in milliseconds per stage.
	// Present only when the request carried ?timing=1; see docs/http-api.md.
	Timing map[string]any `json:"timing,omitempty"`
	// UncountedSessions is how many rows carry a message_count that is not a
	// count. The summary view answers with whatever counts are already cached
	// and declines to parse the rest, so it is usually partly counted rather
	// than wholly uncounted, and a client deciding whether it can filter on
	// message_count needs the number rather than the boolean above.
	UncountedSessions int `json:"uncounted_sessions,omitempty"`
}

// historySummaryListing aggregates the per-row degradation the store attached,
// the same way integrations.HistoryStore.List does for the full listing, so the
// two views of /api/history can never disagree about what they lost. The
// summary view used to build its body by hand and leave `unreadable_sessions`
// and `skipped_records` at zero on every response.
// sendHistoryListing answers both history views. They degrade one row at a
// time (integrations' markUnreadable) and cannot fail wholesale:
// HistoryStore.list returns a nil error unconditionally, so History and
// SearchSessions do too. The 500 branches that used to stand here were the
// last trace of the old wholesale-failure behaviour and were unreachable.
func (s *Server) sendHistoryListing(
	response http.ResponseWriter, request *http.Request, corsOrigin string,
	live []state.SessionInfo, timer *stageTimer,
) {
	archived := s.archivedSessionIDs(request.Context())
	timer.mark("archived")
	listing := historyListResponse{}
	if request.URL.Query().Get("summary") == "true" {
		sessions, _ := s.integrationEndpoints.SearchSessions(live)
		timer.mark("store")
		markArchived(sessions, archived)
		listing = historySummaryListing(sessions)
	} else {
		history, _ := s.integrationEndpoints.History(live)
		timer.mark("store")
		markArchived(history.Sessions, archived)
		listing = historyListResponse{HistoryResponse: history}
	}
	// A caller that asked for the breakdown gets the same numbers the log line
	// carries. It is opt-in because it is diagnosis, not part of a listing.
	if request.URL.Query().Get("timing") == "1" {
		listing.Timing = timer.breakdown()
	}
	s.sendJSON(response, http.StatusOK, listing, corsOrigin)
	timer.mark("encode")
	timer.logIfSlow("listing")
}

// refreshIntegrations tells the integrations service what is running before it
// answers anything, which is what keeps a listing's rows in step with the live
// sessions rather than a poll behind them.
func (s *Server) refreshIntegrations(live []state.SessionInfo, timer *stageTimer) {
	for _, info := range live {
		if session, ok := s.registry.Get(info.ID); ok {
			if err := s.integrationEndpoints.TrackSession(session); err != nil {
				log.Printf("[integrations] track runner %s: %v", info.ID, err)
			}
		}
	}
	timer.mark("track")
	if err := s.integrationEndpoints.ObserveFailures(live); err != nil {
		log.Printf("[integrations] observe runner failures: %v", err)
	}
	timer.mark("observe")
}

// liveSessions lists the sessions once and records where that time went. A
// runtime that can break its own listing down reports ledger, restore-marker
// and process-probe time separately, which is the difference between "the
// listing was slow" and knowing which of them to fix.
type listTimer interface {
	ListTimed(bool) ([]state.SessionInfo, sessionruntime.ListTiming)
}

func (s *Server) liveSessions(timer *stageTimer) []state.SessionInfo {
	runtime, ok := s.registry.(listTimer)
	if !ok {
		live := s.registry.List(true)
		timer.mark("live")
		return live
	}
	live, timing := runtime.ListTimed(true)
	timer.markFor("ledger", timing.Ledger)
	timer.note("ledger_cached", timing.LedgerCached)
	timer.markFor("restores", timing.Restores)
	timer.markFor("probes", timing.Reality)
	timer.mark("live")
	return live
}

var _ listTimer = (*sessionruntime.Manager)(nil)

// WarmHistory pays the first listing's cost before anybody asks for it.
//
// The history store's first pass counts and indexes what it has not seen since
// this process started; measured against the owner's own directories that is
// 0.75 s against 0.16 s for every listing after it. Doing it at startup rather
// than inside the first request is the difference between a person waiting and
// a daemon working while nobody is looking.
//
// It runs in the background on purpose: a daemon must serve immediately, and a
// machine with a very large history would otherwise hold the listener closed
// for as long as the scan takes. A request that arrives first simply does the
// work itself, exactly as it does today.
func (s *Server) WarmHistory(logf func(string, ...any)) {
	go func() {
		started := time.Now()
		_, _ = s.integrationEndpoints.History(s.registry.List(true))
		if took := time.Since(started); took > slowListingThreshold && logf != nil {
			logf("[history] warmed the listing cache in %s", round(took))
		}
	}()
}

type archivedLanesService interface {
	ArchivedSessionIDs(context.Context) ([]string, error)
}

// The manager is what answers this in production. Asserting it here means a
// drifting signature is a build failure rather than a runtime that quietly
// stops marking anything.
var _ archivedLanesService = (*sessionruntime.Manager)(nil)

// archivedSessionIDs is which sessions the person archived, or none when this
// runtime cannot say. A failure to read is logged and answered as none: an
// unmarked row is what every client already handles, and inventing the flag
// either way would be a claim about somebody's history that nothing checked.
func (s *Server) archivedSessionIDs(ctx context.Context) []string {
	manager, ok := s.registry.(archivedLanesService)
	if !ok {
		return nil
	}
	ids, err := manager.ArchivedSessionIDs(ctx)
	if err != nil {
		log.Printf("[integrations] read archived sessions: %v", err)
		return nil
	}
	return ids
}

// markArchived says which of these conversations the person archived. The rows
// are in History on purpose: archiving hides a session from the list without
// deleting anything, so History keeps offering the conversation and names its
// state rather than presenting it as though nothing had happened.
func markArchived(sessions []integrations.HistorySession, archived []string) {
	if len(archived) == 0 {
		return
	}
	hidden := make(map[string]struct{}, len(archived))
	for _, id := range archived {
		hidden[id] = struct{}{}
	}
	for index := range sessions {
		if _, ok := hidden[sessions[index].ID]; ok {
			sessions[index].Archived = true
		}
	}
}

func historySummaryListing(sessions []integrations.HistorySession) historyListResponse {
	listing := historyListResponse{
		HistoryResponse: integrations.HistoryResponse{
			SchemaVersion: integrations.SchemaVersion, Sessions: sessions,
		},
		TranscriptsUnread: true,
	}
	for _, session := range sessions {
		if session.Unreadable {
			listing.UnreadableSessions++
		}
		if session.MessageCountUncounted {
			listing.UncountedSessions++
		}
		listing.SkippedRecords += session.SkippedRecords
	}
	return listing
}

func historyPath(path string) (id, variant string, ok bool) {
	const prefix = "/api/history/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(path, prefix)
	for _, candidate := range []string{"raw", "source", "preview", "window"} {
		if strings.HasSuffix(rest, "/"+candidate) {
			rest = strings.TrimSuffix(rest, "/"+candidate)
			variant = candidate
			break
		}
	}
	if rest == "" || strings.Contains(rest, "/") {
		return "", "", false
	}
	return rest, variant, true
}

type historyQueryError struct{ message string }

func (e *historyQueryError) Error() string { return e.message }

func transcriptWindowOptions(request *http.Request) (integrations.TranscriptWindowOptions, error) {
	query := request.URL.Query()
	options := integrations.TranscriptWindowOptions{End: -1}
	if raw := query.Get("start"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return integrations.TranscriptWindowOptions{}, &historyQueryError{message: "start must be a non-negative message index"}
		}
		options.Start = value
	}
	if raw := query.Get("end"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return integrations.TranscriptWindowOptions{}, &historyQueryError{message: "end must be a non-negative message index"}
		}
		options.End = value
	}
	if options.End >= 0 && options.End < options.Start {
		return integrations.TranscriptWindowOptions{}, &historyQueryError{message: "end must be greater than or equal to start"}
	}
	if options.End >= 0 && options.End-options.Start > integrations.MaxTranscriptWindowSpan {
		return integrations.TranscriptWindowOptions{}, &historyQueryError{
			message: "a transcript window can contain at most " + strconv.Itoa(integrations.MaxTranscriptWindowSpan) + " message positions",
		}
	}
	options.Role = strings.ToLower(strings.TrimSpace(query.Get("role")))
	if options.Role != "" && options.Role != "user" && options.Role != "assistant" && options.Role != "tool" {
		return integrations.TranscriptWindowOptions{}, &historyQueryError{message: "role must be user, assistant, or tool"}
	}
	options.ExpectedMessage = strings.TrimSpace(query.Get("message_id"))
	if options.ExpectedMessage != "" {
		value, err := strconv.Atoi(query.Get("anchor"))
		if err != nil || value < 0 {
			return integrations.TranscriptWindowOptions{}, &historyQueryError{message: "anchor must be a non-negative message index when message_id is set"}
		}
		options.ExpectedIndex = value
	}
	return options, nil
}

func errorsSince(request *http.Request) (uint64, error) {
	raw := request.URL.Query().Get("since")
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, errors.New("since must be a non-negative integer sequence")
	}
	return value, nil
}

func (s *Server) integrationError(response http.ResponseWriter, corsOrigin, summary string, err error) {
	if _, recordErr := s.integrationEndpoints.Emit(integrations.ErrorInput{
		Kind: "daemon_error", Summary: summary, Detail: err.Error(),
	}); recordErr != nil {
		log.Printf("[integrations] record daemon error: %v", recordErr)
	}
	s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
}

func (s *Server) sendIntegrationBytes(
	response http.ResponseWriter,
	status int,
	contentType string,
	body []byte,
	corsOrigin string,
) {
	response.Header().Set("Content-Type", contentType)
	// The same CORS answer sendJSON gives. This helper used to advertise a
	// narrower method list and a shorter allowed-header list, so a browser
	// client that preflighted against a transcript download learned different
	// rules than one that preflighted against any JSON route.
	setCORSHeaders(response, corsOrigin)
	response.Header().Set("Access-Control-Expose-Headers", "X-Sessions-Schema-Version")
	response.WriteHeader(status)
	_, _ = response.Write(body)
}

func formatTranscriptText(transcript integrations.TranscriptResponse) string {
	var output strings.Builder
	for index, message := range transcript.Messages {
		if index > 0 {
			output.WriteByte('\n')
		}
		fmt.Fprintf(&output, "[%s", message.Role)
		if message.Timestamp != nil {
			fmt.Fprintf(&output, " %s", *message.Timestamp)
		}
		output.WriteString("]\n")
		output.WriteString(message.Text)
		output.WriteByte('\n')
	}
	return output.String()
}
