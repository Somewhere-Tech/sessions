package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/webassets"
)

func setCORSHeaders(response http.ResponseWriter, corsOrigin string) {
	if corsOrigin != "" {
		response.Header().Set("Access-Control-Allow-Origin", corsOrigin)
	}
	response.Header().Set("Vary", "Origin")
	response.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
	response.Header().Set("Access-Control-Allow-Headers", "content-type, authorization, x-sessions-creator-session, x-sessions-owner-id, x-sessions-client, x-sessions-filename, x-sessions-user-consent")
}

func (s *Server) sendJSON(response http.ResponseWriter, status int, body any, corsOrigin string) {
	response.Header().Set("Content-Type", "application/json")
	setCORSHeaders(response, corsOrigin)
	response.WriteHeader(status)
	if status == http.StatusNoContent {
		return
	}
	_ = json.NewEncoder(response).Encode(body)
}

func captureCreatorHeaders(request *http.Request, body *state.CreateSessionRequest) error {
	sessionID, hasSession, err := creatorHeaderValue(request.Header, creatorSessionHeader)
	if err != nil {
		return err
	}
	ownerID, hasOwner, err := creatorHeaderValue(request.Header, creatorOwnerHeader)
	if err != nil {
		return err
	}
	if hasSession && hasOwner {
		return errors.New("creator session and external owner headers cannot be combined")
	}
	body.CreatorSessionID = sessionID
	body.CreatorOwnerID = ownerID
	return nil
}

func captureInputAttribution(request *http.Request) (state.InputAttribution, bool, error) {
	sessionID, hasSession, err := creatorHeaderValue(request.Header, creatorSessionHeader)
	if err != nil {
		return state.InputAttribution{}, false, err
	}
	_, hasOwner, err := creatorHeaderValue(request.Header, creatorOwnerHeader)
	if err != nil {
		return state.InputAttribution{}, false, err
	}
	if hasSession && hasOwner {
		return state.InputAttribution{}, false, errors.New("source session and external owner headers cannot be combined")
	}
	if hasOwner {
		return state.InputAttribution{}, false, errors.New("external owner attribution is not supported for session input")
	}
	if !hasSession {
		return state.InputAttribution{}, false, nil
	}
	principal, ok := request.Context().Value(authPrincipalContextKey{}).(authPrincipal)
	if !ok {
		return state.InputAttribution{}, false, errors.New("authenticated principal is unavailable")
	}
	if !principal.Local {
		return state.InputAttribution{}, false, errors.New("source session attribution is only accepted by the source machine")
	}
	client := strings.TrimSpace(request.Header.Get(endClientHeader))
	if client == "" {
		client = "api"
	}
	if utf8.RuneCountInString(client) > 64 || strings.IndexFunc(client, unicode.IsControl) >= 0 {
		return state.InputAttribution{}, false, errors.New("X-Sessions-Client is invalid")
	}
	return state.InputAttribution{SourceSessionID: sessionID, Client: client}, true, nil
}

func (s *Server) writeSessionInput(
	ctx context.Context,
	id, data string,
	attribution state.InputAttribution,
	attributed bool,
) error {
	if !attributed {
		if s.registry.Input(ctx, id, data) {
			return nil
		}
		return &sessionruntime.MessageInputUnavailableError{SessionID: id}
	}
	service, supported := s.registry.(attributedInputService)
	if !supported {
		return errors.New("message attribution is unavailable")
	}
	return service.InputAttributed(ctx, id, data, attribution)
}

func (s *Server) sendInputError(response http.ResponseWriter, err error, corsOrigin string) {
	var invalid *sessionruntime.InvalidMessageSourceError
	var invalidInput *sessionruntime.InvalidAttributedInputError
	var unavailable *sessionruntime.MessageInputUnavailableError
	var committed *sessionruntime.MessageAttributionCommitError
	switch {
	case errors.As(err, &invalid), errors.As(err, &invalidInput):
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
	case errors.As(err, &unavailable):
		s.sendJSON(response, http.StatusNotFound, map[string]any{"error": err.Error()}, corsOrigin)
	case errors.As(err, &committed):
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{
			"error": err.Error(), "delivered": true, "retry": false,
		}, corsOrigin)
	default:
		status := http.StatusInternalServerError
		if err.Error() == "message attribution is unavailable" {
			status = http.StatusNotImplemented
		}
		s.sendJSON(response, status, map[string]any{"error": err.Error()}, corsOrigin)
	}
}

func captureEndRequest(request *http.Request, reason, operationID string) (state.EndSessionRequest, error) {
	principal, ok := request.Context().Value(authPrincipalContextKey{}).(authPrincipal)
	if !ok {
		return state.EndSessionRequest{}, errors.New("authenticated principal is unavailable")
	}
	sessionID, hasSession, err := creatorHeaderValue(request.Header, creatorSessionHeader)
	if err != nil {
		return state.EndSessionRequest{}, err
	}
	ownerID, hasOwner, err := creatorHeaderValue(request.Header, creatorOwnerHeader)
	if err != nil {
		return state.EndSessionRequest{}, err
	}
	if hasSession && hasOwner {
		return state.EndSessionRequest{}, errors.New("initiator session and external owner headers cannot be combined")
	}
	client := strings.TrimSpace(request.Header.Get(endClientHeader))
	if client == "" {
		client = "api"
	}
	reason = strings.TrimSpace(reason)
	operationID = strings.TrimSpace(operationID)
	for _, field := range []struct {
		name  string
		value string
		limit int
	}{
		{"initiator name", principal.Name, 128},
		{"client", client, 64},
		{"reason", reason, 280},
		{"operation id", operationID, 128},
	} {
		if utf8.RuneCountInString(field.value) > field.limit {
			return state.EndSessionRequest{}, fmt.Errorf("%s exceeds %d characters", field.name, field.limit)
		}
		if strings.IndexFunc(field.value, unicode.IsControl) >= 0 {
			return state.EndSessionRequest{}, fmt.Errorf("%s must not contain control characters", field.name)
		}
	}
	end := state.EndSessionRequest{
		InitiatorKind: string(principal.Kind),
		InitiatorID:   principal.ID,
		InitiatorName: principal.Name,
		Client:        client,
		Reason:        reason,
		OperationID:   operationID,
	}
	if principal.Local && hasSession {
		end.InitiatorKind = string(ledger.CreatorSession)
		end.InitiatorID = sessionID
		end.InitiatorName = ""
	} else if principal.Local && hasOwner {
		end.InitiatorKind = string(ledger.CreatorExternal)
		end.InitiatorID = ownerID
		end.InitiatorName = ""
	}
	return end, nil
}

func creatorHeaderValue(header http.Header, name string) (string, bool, error) {
	values, present := header[http.CanonicalHeaderKey(name)]
	if !present {
		return "", false, nil
	}
	if len(values) != 1 || values[0] == "" {
		return "", true, fmt.Errorf("%s must contain exactly one non-empty value", name)
	}
	return values[0], true, nil
}

// checkJSONContentType is the content-type guard every JSON POST shares. It is
// separate from readJSON so routes that must decode with their own decoder
// still enforce the same rule.
func checkJSONContentType(request *http.Request) error {
	if request.ContentLength == 0 {
		return nil
	}
	contentTypes := request.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		return errors.New("content-type must be application/json")
	}
	mediaType, _, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || mediaType != "application/json" {
		return errors.New("content-type must be application/json")
	}
	return nil
}

func readJSON(request *http.Request, target any) error {
	if err := checkJSONContentType(request); err != nil {
		return err
	}
	reader := http.MaxBytesReader(nil, request.Body, maxJSONBody)
	encoded, err := io.ReadAll(reader)
	if err != nil {
		return jsonRequestError(err)
	}
	if !utf8.Valid(encoded) {
		return errors.New("request body must be valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	if err := decoder.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return jsonRequestError(err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain a single JSON value")
		}
		return jsonRequestError(err)
	}
	return nil
}

func jsonRequestError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return errors.New("request body too large")
	}
	return err
}

func sessionRoute(path string) (id, suffix string, ok bool) {
	const prefix = "/api/sessions/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(path, prefix)
	parts := strings.SplitN(rest, "/", 2)
	if parts[0] == "" {
		return "", "", false
	}
	decoded, err := url.PathUnescape(parts[0])
	if err != nil {
		return "", "", false
	}
	suffix = ""
	if len(parts) == 2 {
		suffix = "/" + parts[1]
	}
	return decoded, suffix, true
}

func queryIndex(values url.Values, key string) *int64 {
	raw, present := values[key]
	if !present || len(raw) == 0 {
		return nil
	}
	value, err := strconv.ParseFloat(raw[0], 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	integer := int64(value)
	return &integer
}

func isStaticRequest(path, method string) bool {
	return method == http.MethodGet && !strings.HasPrefix(path, "/api/") && path != "/api" && !strings.HasPrefix(path, "/ws")
}

func (s *Server) serveStatic(response http.ResponseWriter, request *http.Request) bool {
	root, err := os.OpenRoot(s.config.WebDir)
	if err != nil {
		return webassets.ServeHTTP(response, request)
	}
	defer root.Close()
	escaped := request.URL.EscapedPath()
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "invalid path"}, "")
		return true
	}
	relative := strings.TrimLeft(decoded, "/")
	normalized := filepath.Clean(relative)
	if normalized == "." {
		normalized = ""
	}
	if normalized == ".." || strings.HasPrefix(normalized, ".."+string(filepath.Separator)) || filepath.IsAbs(normalized) {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "invalid path"}, "")
		return true
	}
	canonicalRoot := canonicalPath(s.config.WebDir)
	canonicalCandidate := canonicalPath(filepath.Join(canonicalRoot, normalized))
	if !pathWithinBase(canonicalCandidate, canonicalRoot) {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "invalid path"}, "")
		return true
	}
	opened, fileInfo := readableRootFile(root, normalized)
	if opened == nil {
		opened, fileInfo = readableRootFile(root, "index.html")
	}
	if opened == nil {
		return false
	}
	defer opened.Close()
	http.ServeContent(response, request, filepath.Base(opened.Name()), fileInfo.ModTime(), opened)
	return true
}

func readableRootFile(root *os.Root, name string) (*os.File, os.FileInfo) {
	if name == "" {
		name = "."
	}
	opened, err := root.Open(name)
	if err != nil {
		return nil, nil
	}
	info, err := opened.Stat()
	if err != nil {
		_ = opened.Close()
		return nil, nil
	}
	if info.Mode().IsRegular() {
		return opened, info
	}
	_ = opened.Close()
	if info.IsDir() {
		index, err := root.Open(filepath.Join(name, "index.html"))
		if err == nil {
			indexInfo, statErr := index.Stat()
			if statErr == nil && indexInfo.Mode().IsRegular() {
				return index, indexInfo
			}
			_ = index.Close()
		}
	}
	return nil, nil
}

func (s *Server) pendingRestore(id string) (state.RestorePending, bool) {
	reporter, ok := s.registry.(pendingRestoreService)
	if !ok {
		return state.RestorePending{}, false
	}
	return reporter.PendingRestore(id)
}

func (s *Server) sendPendingRestore(response http.ResponseWriter, id, corsOrigin string) bool {
	pending, ok := s.pendingRestore(id)
	if !ok {
		return false
	}
	reason := strings.TrimSpace(pending.Reason)
	if reason == "" {
		reason = "the runner stayed paused after reboot"
	}
	action := "sessions resume " + id
	s.sendJSON(response, http.StatusConflict, map[string]any{
		"code": "SESSION_NEEDS_RECREATE", "sessionId": id,
		"error":  "session is paused after reboot and cannot be read or controlled until it is resumed: " + reason,
		"action": action,
	}, corsOrigin)
	return true
}

// wakePausedSession restarts a reboot-paused session on first contact and
// returns it live. False means it is not paused, or waking failed; the
// caller then reports the paused state with the failure as its reason.
func (s *Server) wakePausedSession(ctx context.Context, id string) (*state.Session, bool, error) {
	if _, paused := s.pendingRestore(id); !paused {
		return nil, false, nil
	}
	waker, ok := s.registry.(pausedWaker)
	if !ok {
		return nil, false, nil
	}
	if _, err := waker.WakePaused(ctx, id); err != nil {
		return nil, false, err
	}
	session, live := s.registry.Get(id)
	return session, live, nil
}

// sessionOnContact returns a live session, waking a reboot-paused one when
// needed. WebSocket reads report any remaining paused state to the peer, so
// they need the live result rather than a separate wake error.
func (s *Server) sessionOnContact(ctx context.Context, id string) (*state.Session, bool) {
	if session, live := s.registry.Get(id); live {
		return session, true
	}
	session, live, _ := s.wakePausedSession(ctx, id)
	return session, live
}
