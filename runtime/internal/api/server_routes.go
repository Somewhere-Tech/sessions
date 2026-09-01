package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/delivery"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

func (s *Server) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	path := request.URL.Path
	origin := request.Header.Get("Origin")
	corsOrigin := ""
	originAllowed := allowedOrigin(origin, s.config.Host, s.lan.activeHost())
	if originAllowed {
		corsOrigin = origin
	}

	// CORS controls whether a browser may read a response; it does not stop a
	// browser from sending a state-changing request. Reject ambient-authority
	// writes from untrusted browser origins before authentication or route
	// dispatch. Credential-bearing remote clients remain valid; a hostile page
	// cannot add Authorization without a preflight that this server rejects.
	if isStateChangingMethod(request.Method) && origin != "" &&
		!trustedAmbientWriteOrigin(origin, s.config.Host, s.config.Port, s.lan.activeHost()) &&
		strings.TrimSpace(request.Header.Get("Authorization")) == "" {
		s.sendJSON(response, http.StatusForbidden, map[string]any{"error": "forbidden origin"}, "")
		return
	}

	if request.Method == http.MethodOptions {
		s.sendJSON(response, http.StatusNoContent, map[string]any{}, corsOrigin)
		return
	}
	if isStaticRequest(path, request.Method) {
		if !s.serveStatic(response, request) {
			s.sendJSON(response, http.StatusNotFound, map[string]any{
				"error": "web build not found", "path": s.config.WebDir,
			}, corsOrigin)
		}
		return
	}
	if path == "/api/health" && request.Method == http.MethodGet {
		// /api/health stays unauthenticated on purpose: native discovery,
		// `sessions machines discover`, the updater, and the frontend's
		// bootstrapCurrentOriginServer all depend on it, the last on the
		// 200-vs-401 distinction. The selected private IPv4 and port are a
		// different matter — they map the user's network for anyone who can
		// reach the port — so that one field is redacted unless the caller
		// has already proved it belongs here. `lan.enabled` stays visible so
		// probes can still tell whether the listener is up.
		lanState := s.lan.state()
		if !s.mayReadLANEndpoint(request) {
			lanState.URL = nil
		}
		restore := s.rebootRestoreHealth()
		s.sendJSON(response, http.StatusOK, map[string]any{
			"ok": true, "name": "sessionsd", "version": Version,
			"status": restore["status"],
			"listen": map[string]any{"host": s.config.Host, "port": s.config.Port},
			"lan":    lanState,
			"access": map[string]any{"open": s.openAccessEnabled()},
			"system": map[string]any{"os": goruntime.GOOS, "arch": goruntime.GOARCH},
			"compatibility": map[string]any{
				"api": map[string]any{
					"current": apiProtocolVersion, "minimumClient": minimumAPIClient, "maximumClient": maximumAPIClient,
				},
				"runner": map[string]any{
					"current": proto.ProtocolVersion, "minimum": proto.MinimumCompatibleVersion, "maximum": proto.MaximumCompatibleVersion,
				},
			},
			"discovering":    s.registry.IsDiscovering(),
			"sessionsLoaded": len(s.registry.List(true)),
			"restore":        restore,
		}, corsOrigin)
		return
	}
	if s.handlePairClaimRoute(response, request, corsOrigin) {
		return
	}
	if s.handleTailnetAccessPublicRoute(response, request, corsOrigin) {
		return
	}
	if s.handleNearbyAccessPublicRoute(response, request, corsOrigin) {
		return
	}

	principal, authorized, err := s.authorized(request)
	if err != nil {
		// This runs before the caller has proved anything, and the detail is
		// usually an os.PathError naming the token file — an absolute path
		// that spells out the state directory and the user's account name.
		// The operator gets the whole error in the daemon log and over
		// loopback; an unauthenticated peer gets the next action and nothing
		// to enumerate.
		log.Printf("sessionsd: authorization check failed for %s %s: %v", request.Method, path, err)
		detail := "Sessions could not read this machine's auth token, so no request can be authorized. Check the sessionsd log, then restart the daemon."
		if isLoopbackPeer(request) {
			detail = err.Error()
		}
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": detail}, corsOrigin)
		return
	}
	if !authorized {
		s.sendJSON(response, http.StatusUnauthorized, map[string]any{"error": "unauthorized"}, corsOrigin)
		return
	}
	request = request.WithContext(context.WithValue(request.Context(), authPrincipalContextKey{}, principal))
	// Deep health is dispatched after authorization, unlike plain /api/health.
	// It reports live session UUIDs and host PIDs, so an unauthenticated peer
	// on the LAN listener or the Tailscale Serve frontend could otherwise
	// enumerate the user's running work. Its first-party caller is
	// `sessions doctor`, which reaches the daemon over loopback (or with a
	// per-device token when targeting another machine).
	if path == "/api/health/deep" && request.Method == http.MethodGet {
		restore := s.rebootRestoreHealth()
		s.sendJSON(response, http.StatusOK, map[string]any{
			"ok": true, "name": "sessionsd", "version": Version,
			"status": restore["status"],
			"access": map[string]any{"open": s.openAccessEnabled()},
			"compatibility": map[string]any{
				"api": map[string]any{
					"current": apiProtocolVersion, "minimumClient": minimumAPIClient, "maximumClient": maximumAPIClient,
				},
				"runner": map[string]any{
					"current": proto.ProtocolVersion, "minimum": proto.MinimumCompatibleVersion, "maximum": proto.MaximumCompatibleVersion,
				},
			},
			"discovering":    s.registry.IsDiscovering(),
			"sessionsLoaded": len(s.registry.List(true)),
			"restore":        restore,
			"uptimeSec":      int64(math.Round(s.registry.Uptime().Seconds())),
			"sessions":       s.registry.DeepDiagnostics(),
		}, corsOrigin)
		return
	}
	if path == "/api/machine" && request.Method == http.MethodGet {
		if s.identityError != nil || s.identity.ID == "" {
			detail := "machine identity is unavailable"
			if s.identityError != nil {
				detail = s.identityError.Error()
			}
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": detail}, corsOrigin)
			return
		}
		name, err := os.Hostname()
		name = truncateMachineName(name)
		if err != nil || name == "" {
			name = s.identity.Name
		}
		s.sendJSON(response, http.StatusOK, map[string]any{
			"machine_id": s.identity.ID,
			"name":       name,
		}, corsOrigin)
		return
	}
	if path == "/ws" {
		if !allowedOrigin(origin, s.config.Host, s.lan.activeHost()) {
			s.sendJSON(response, http.StatusForbidden, map[string]any{"error": "forbidden origin"}, "")
			return
		}
		writes, err := s.websocketWritesAllowed(request)
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		s.serveWebSocket(response, request, writes)
		return
	}
	if s.handleLANRoute(response, request, corsOrigin) {
		return
	}
	if s.handleNotifyRoute(response, request, corsOrigin) {
		return
	}
	if s.handlePairRoutes(response, request, corsOrigin) {
		return
	}
	if s.handleTailnetAccessAdminRoute(response, request, corsOrigin) {
		return
	}
	if path == "/api/push/vapid" && request.Method == http.MethodGet {
		publicKey, err := s.push.VAPIDPublicKey()
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"publicKey": publicKey}, corsOrigin)
		return
	}
	if path == "/api/push/subscribe" && request.Method == http.MethodPost {
		var body any
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		if err := s.push.AddSubscription(body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"ok": true}, corsOrigin)
		return
	}
	if path == "/api/push/unsubscribe" && request.Method == http.MethodPost {
		var body map[string]any
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		endpoint, ok := body["endpoint"].(string)
		if !ok || endpoint == "" {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "endpoint is required"}, corsOrigin)
			return
		}
		if err := s.push.RemoveSubscription(endpoint); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"ok": true}, corsOrigin)
		return
	}
	if path == "/api/sessions" && request.Method == http.MethodGet {
		includeExited := request.URL.Query().Get("include_exited") == "1"
		s.sendJSON(response, http.StatusOK, map[string]any{"sessions": s.registry.List(includeExited)}, corsOrigin)
		return
	}
	if path == "/api/models/codex" && request.Method == http.MethodGet {
		catalog, supported := s.registry.(newSessionModelCatalogService)
		if !supported {
			s.sendJSON(response, http.StatusNotImplemented, map[string]any{"error": "Codex model choices are not available on this runtime"}, corsOrigin)
			return
		}
		models, err := catalog.CodexModelOptions(request.Context())
		if err != nil {
			s.sendJSON(response, http.StatusBadGateway, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"models": models}, corsOrigin)
		return
	}
	if path == "/api/sessions/end-batch" && request.Method == http.MethodPost {
		var body struct {
			IDs         []string `json:"ids"`
			Reason      string   `json:"reason,omitempty"`
			OperationID string   `json:"operationId,omitempty"`
			Force       bool     `json:"force,omitempty"`
		}
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		if len(body.IDs) < 2 {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "batch end requires at least two session ids"}, corsOrigin)
			return
		}
		for _, id := range body.IDs {
			if strings.TrimSpace(id) == "" {
				s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "session ids must not be empty"}, corsOrigin)
				return
			}
			if _, ok := s.registry.Get(id); !ok {
				s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "session not found", "id": id}, corsOrigin)
				return
			}
		}
		end, err := captureEndRequest(request, body.Reason, body.OperationID)
		if err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		batch, supported := s.registry.(attributedBatchKillService)
		if !supported {
			s.sendJSON(response, http.StatusNotImplemented, map[string]any{"error": "batch end is unavailable"}, corsOrigin)
			return
		}
		if err := batch.KillManyAttributed(request.Context(), body.IDs, body.Force, end); err != nil {
			status := http.StatusInternalServerError
			var guard *sessionruntime.MassKillError
			if errors.As(err, &guard) {
				status = http.StatusConflict
			}
			s.sendJSON(response, status, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"ok": true, "ids": body.IDs}, corsOrigin)
		return
	}
	if s.handleRetentionRoute(response, request, corsOrigin) {
		return
	}
	if s.handleMoveRoute(response, request, corsOrigin) {
		return
	}
	if s.handleBackupRoute(response, request, corsOrigin) {
		return
	}
	if s.handleIntegrationsRoute(response, request, corsOrigin) {
		return
	}
	if s.handleSearchRoute(response, request, corsOrigin) {
		return
	}
	if s.handleUsageRoute(response, request, corsOrigin) {
		return
	}
	if s.handleRecapRoute(response, request, corsOrigin) {
		return
	}
	if s.handleOnboardingRoute(response, request, corsOrigin) {
		return
	}
	if s.handleClaudeSettingsRoute(response, request, corsOrigin) {
		return
	}
	if s.handleProvidersRoute(response, request, corsOrigin) {
		return
	}
	if s.handleProfilesRoute(response, request, corsOrigin) {
		return
	}
	if s.handleWorktreesRoute(response, request, corsOrigin) {
		return
	}
	if s.handleLanesRoute(response, request, corsOrigin) {
		return
	}
	if path == "/api/recovery" || path == "/api/recovery/reopen" ||
		path == "/api/recovery/adopt" || path == "/api/recovery/fork" {
		s.handleRecovery(response, request, corsOrigin)
		return
	}
	if path == "/api/directories" && request.Method == http.MethodGet {
		s.sendJSON(response, http.StatusOK, map[string]any{"directories": listDirectoryCandidates(s.registry.List(true))}, corsOrigin)
		return
	}
	if path == "/api/fs/list" && request.Method == http.MethodGet {
		s.handleFSList(response, request, corsOrigin)
		return
	}
	if path == "/api/sessions" && request.Method == http.MethodPost {
		var body state.CreateSessionRequest
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		if err := captureCreatorHeaders(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		info, err := s.registry.Create(request.Context(), body)
		if err != nil {
			status := http.StatusBadRequest
			var live *sessionruntime.ConversationLiveError
			var moved *sessionruntime.ConversationMovedError
			if errors.As(err, &live) || errors.As(err, &moved) {
				status = http.StatusConflict
			}
			s.sendJSON(response, status, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		s.sendJSON(response, http.StatusCreated, info, corsOrigin)
		return
	}
	if path == "/api/claude-sessions" && request.Method == http.MethodGet {
		// Preserve the original endpoint shape for older clients. The provider
		// fields live only on the generalized resumable-conversations route.
		scanned := watch.ScanResumableSessions()
		legacy := make([]map[string]any, 0, len(scanned))
		for _, session := range scanned {
			legacy = append(legacy, map[string]any{
				"sessionId": session.SessionID, "cwd": session.Cwd,
				"modifiedAt": session.ModifiedAt, "firstUserMessage": session.FirstUserMessage,
				"sizeBytes": session.SizeBytes,
			})
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"sessions": legacy}, corsOrigin)
		return
	}
	if path == "/api/resumable-conversations" && request.Method == http.MethodGet {
		s.sendJSON(response, http.StatusOK, s.resumableConversations(), corsOrigin)
		return
	}
	if s.handleDeliveryRoute(response, request, corsOrigin) {
		return
	}

	id, suffix, matched := sessionRoute(path)
	if matched {
		if s.handleVerdictRoute(response, request, id, suffix, corsOrigin) {
			return
		}
		if s.handleWaitRoute(response, request, id, suffix, corsOrigin) {
			return
		}
		if s.handlePinRoute(response, request, id, suffix, corsOrigin) {
			return
		}
		s.handleSessionRoute(response, request, id, suffix, corsOrigin)
		return
	}
	s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "not found", "path": path}, corsOrigin)
}

func (s *Server) rebootRestoreHealth() map[string]any {
	pending := 0
	if reporter, ok := s.registry.(rebootRestoreHealthService); ok {
		pending = reporter.RestorePendingCount()
	}
	health := map[string]any{
		"pending":              pending,
		"automaticPinnedLimit": state.DefaultPinnedBootRestoreLimit,
		"degraded":             pending > 0,
		"status":               "healthy",
	}
	if pending > 0 {
		health["status"] = "degraded"
		health["code"] = "SESSION_RESTORE_PENDING"
		health["message"] = fmt.Sprintf("%d session(s) are paused after reboot and need recovery", pending)
		health["action"] = "sessions doctor"
	}
	return health
}

func (s *Server) authorized(request *http.Request) (authPrincipal, bool, error) {
	localUserID, err := ledger.LocalUserCreatorID()
	if err != nil {
		return authPrincipal{}, false, err
	}
	localUser := authPrincipal{
		Local:     true,
		HostAdmin: true,
		Kind:      ledger.CreatorUser,
		ID:        localUserID,
		Name:      "Local user",
	}
	if isLoopbackPeer(request) {
		return localUser, true, nil
	}
	if _, err := os.Stat(s.config.OpenPath); err == nil {
		return authPrincipal{Kind: ledger.CreatorExternal, ID: "remote:open-access", Name: "Remote open access"}, true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return authPrincipal{}, false, err
	}
	expected, err := s.tokens.token()
	if err != nil {
		return authPrincipal{}, false, err
	}
	authorization := request.Header.Get("Authorization")
	if strings.HasPrefix(authorization, "Bearer ") && tokenEqual(strings.TrimPrefix(authorization, "Bearer "), expected) {
		return authPrincipal{HostAdmin: true, Kind: ledger.CreatorExternal, ID: "remote:master-token", Name: "Remote administrator"}, true, nil
	}
	if strings.HasPrefix(authorization, "Bearer ") {
		if device, authorized, err := s.pair.devices.authenticate(strings.TrimPrefix(authorization, "Bearer ")); authorized || err != nil {
			if err != nil {
				return authPrincipal{}, false, err
			}
			return authPrincipal{
				HostAdmin: true, Kind: ledger.CreatorExternal, ID: "device:" + device.DeviceID, Name: device.Name,
			}, true, nil
		}
	}
	provided := request.URL.Query().Get("token")
	if provided == "" {
		return authPrincipal{}, false, nil
	}
	if tokenEqual(provided, expected) {
		return authPrincipal{HostAdmin: true, Kind: ledger.CreatorExternal, ID: "remote:master-token", Name: "Remote administrator"}, true, nil
	}
	device, authorized, err := s.pair.devices.authenticate(provided)
	if err != nil || !authorized {
		return authPrincipal{}, authorized, err
	}
	return authPrincipal{
		HostAdmin: true, Kind: ledger.CreatorExternal, ID: "device:" + device.DeviceID, Name: device.Name,
	}, true, nil
}

// mayReadLANEndpoint decides whether a caller of the unauthenticated
// /api/health may see the LAN listener's address. A peer that presents no
// credential at all is never charged the cost of a full authorization check,
// so a flood of anonymous probes cannot make the daemon read the token file or
// touch the device store.
func (s *Server) mayReadLANEndpoint(request *http.Request) bool {
	if isLoopbackPeer(request) {
		return true
	}
	if strings.TrimSpace(request.Header.Get("Authorization")) == "" &&
		strings.TrimSpace(request.URL.Query().Get("token")) == "" {
		return false
	}
	_, authorized, err := s.authorized(request)
	return err == nil && authorized
}

func (s *Server) openAccessEnabled() bool {
	_, err := os.Stat(s.config.OpenPath)
	return err == nil
}

func (s *Server) requireLocalPrincipal(response http.ResponseWriter, request *http.Request, corsOrigin, operation string) bool {
	principal, ok := request.Context().Value(authPrincipalContextKey{}).(authPrincipal)
	if ok && principal.Local {
		return true
	}
	s.sendJSON(response, http.StatusForbidden, map[string]any{
		"error": operation + " is available only on this machine",
	}, corsOrigin)
	return false
}

func (s *Server) handleSessionRoute(response http.ResponseWriter, request *http.Request, id, suffix, corsOrigin string) {
	session, ok := s.registry.Get(id)
	if !ok && sessionRuntimeRoute(request.Method, suffix) && s.sendPendingRestore(response, id, corsOrigin) {
		return
	}
	if suffix == "/model-options" && request.Method == http.MethodGet {
		if !ok {
			s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "unknown session", "id": id}, corsOrigin)
			return
		}
		catalog, supported := s.registry.(modelCatalogService)
		if !supported {
			s.sendJSON(response, http.StatusNotImplemented, map[string]any{"error": "live model options are not available on this runtime"}, corsOrigin)
			return
		}
		models, err := catalog.ModelOptions(request.Context(), id)
		if err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"models": models}, corsOrigin)
		return
	}
	if suffix == "/model" && request.Method == http.MethodPut {
		if !ok {
			s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "unknown session", "id": id}, corsOrigin)
			return
		}
		var body struct {
			Model  string  `json:"model"`
			Effort *string `json:"effort"`
		}
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		controls, supported := s.registry.(modelControlService)
		if !supported {
			s.sendJSON(response, http.StatusNotImplemented, map[string]any{"error": "live model controls are not available on this runtime"}, corsOrigin)
			return
		}
		effort := session.Info().Effort
		if body.Effort != nil {
			effort = *body.Effort
		}
		info, err := controls.ConfigureModel(request.Context(), id, body.Model, effort)
		if err != nil {
			status := http.StatusBadRequest
			switch {
			case errors.Is(err, state.ErrSessionNotFound):
				status = http.StatusNotFound
			case errors.Is(err, state.ErrSessionWorking),
				errors.Is(err, state.ErrSessionEnded),
				errors.Is(err, state.ErrRunnerProtocol):
				status = http.StatusConflict
			}
			s.sendJSON(response, status, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		s.sendJSON(response, http.StatusOK, info, corsOrigin)
		return
	}
	if suffix == "/display-parent" && request.Method == http.MethodPut {
		var body struct {
			ParentSessionID string `json:"parentSessionId"`
		}
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		grouping, supported := s.registry.(interface {
			UpdateDisplayParent(string, string) (string, error)
		})
		if !supported {
			s.sendJSON(response, http.StatusNotImplemented, map[string]any{"error": "session grouping is not available on this runtime"}, corsOrigin)
			return
		}
		parentID, err := grouping.UpdateDisplayParent(id, body.ParentSessionID)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, state.ErrSessionNotFound) {
				status = http.StatusNotFound
			}
			s.sendJSON(response, status, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"displayParentSessionId": parentID}, corsOrigin)
		return
	}
	if suffix == "/set-aside" && request.Method == http.MethodPut {
		var body struct {
			SetAside bool `json:"setAside"`
		}
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		workingSet, supported := s.registry.(interface {
			UpdateSetAside(string, bool) (*int64, error)
		})
		if !supported {
			s.sendJSON(response, http.StatusNotImplemented, map[string]any{"error": "session working-set organization is not available on this runtime"}, corsOrigin)
			return
		}
		setAsideAt, err := workingSet.UpdateSetAside(id, body.SetAside)
		if err != nil {
			status := http.StatusBadRequest
			switch {
			case errors.Is(err, state.ErrSessionNotFound):
				status = http.StatusNotFound
			case errors.Is(err, state.ErrSessionEnded):
				status = http.StatusConflict
			}
			s.sendJSON(response, status, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"setAsideAt": setAsideAt}, corsOrigin)
		return
	}
	if suffix == "/name" && request.Method == http.MethodPut {
		var body struct {
			Name string `json:"name"`
			Auto bool   `json:"auto"`
		}
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		// "auto" gives the name back to the provider's conversation title
		// rather than setting one, so it carries no name of its own.
		if body.Auto {
			releaser, releasable := s.registry.(interface {
				ReleaseName(context.Context, string) (string, error)
			})
			var name string
			var err error
			if releasable {
				name, err = releaser.ReleaseName(request.Context(), id)
			} else if registryReleaser, registryReleasable := s.registry.(interface {
				ReleaseName(string) (string, error)
			}); registryReleasable {
				name, err = registryReleaser.ReleaseName(id)
			} else {
				s.sendJSON(response, http.StatusNotImplemented, map[string]any{"error": "automatic session naming is not available on this runtime"}, corsOrigin)
				return
			}
			if err != nil {
				status := http.StatusBadRequest
				if errors.Is(err, state.ErrSessionNotFound) || errors.Is(err, os.ErrNotExist) {
					status = http.StatusNotFound
				}
				s.sendJSON(response, status, map[string]any{"error": err.Error()}, corsOrigin)
				return
			}
			s.sendJSON(response, http.StatusOK, map[string]any{"name": name}, corsOrigin)
			return
		}
		renamer, supported := s.registry.(interface {
			UpdateName(context.Context, string, string) (string, error)
		})
		var name string
		var err error
		if supported {
			name, err = renamer.UpdateName(request.Context(), id, body.Name)
		} else if registryRenamer, registrySupported := s.registry.(interface {
			UpdateName(string, string) (string, error)
		}); registrySupported {
			name, err = registryRenamer.UpdateName(id, body.Name)
		} else {
			s.sendJSON(response, http.StatusNotImplemented, map[string]any{"error": "session rename is not available on this runtime"}, corsOrigin)
			return
		}
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, state.ErrSessionNotFound) || errors.Is(err, os.ErrNotExist) {
				status = http.StatusNotFound
			}
			s.sendJSON(response, status, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"name": name}, corsOrigin)
		return
	}
	if suffix == "/tags" && request.Method == http.MethodGet {
		tags, err := s.registry.Tags(id)
		if err != nil {
			// The PUT on this same path already distinguishes "no such session"
			// from "this machine could not read it". The GET reported both as
			// 404, so a caller retried forever against a lane that exists.
			if errors.Is(err, state.ErrSessionNotFound) || errors.Is(err, os.ErrNotExist) {
				s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "unknown session", "id": id}, corsOrigin)
				return
			}
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error(), "id": id}, corsOrigin)
			return
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"tags": tags}, corsOrigin)
		return
	}
	if suffix == "/tags" && request.Method == http.MethodPut {
		var body struct {
			Tags map[string]string `json:"tags"`
		}
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		tags, err := s.registry.UpdateTags(id, body.Tags)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, state.ErrSessionNotFound) || errors.Is(err, os.ErrNotExist) {
				status = http.StatusNotFound
			}
			s.sendJSON(response, status, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"tags": tags}, corsOrigin)
		return
	}
	if suffix == "" && request.Method == http.MethodDelete {
		if !ok {
			s.sendJSON(response, http.StatusNotFound, map[string]any{"ok": false}, corsOrigin)
			return
		}
		var body struct {
			Reason      string `json:"reason,omitempty"`
			OperationID string `json:"operationId,omitempty"`
		}
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()}, corsOrigin)
			return
		}
		end, err := captureEndRequest(request, body.Reason, body.OperationID)
		if err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()}, corsOrigin)
			return
		}
		if attributed, supported := s.registry.(attributedKillService); supported {
			err = attributed.RequestKillAttributed(request.Context(), id, request.URL.Query().Get("force") == "1", end)
		} else {
			err = s.registry.RequestKill(request.Context(), id, request.URL.Query().Get("force") == "1")
		}
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()}, corsOrigin)
			return
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"ok": true}, corsOrigin)
		return
	}
	if suffix == "/snapshot" && request.Method == http.MethodGet {
		if !ok {
			s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "unknown session", "id": id}, corsOrigin)
			return
		}
		cols := int(nonnegativeUint(request.URL.Query().Get("cols")))
		if cols < 0 {
			cols = 0
		}
		var text string
		var seq uint32
		var err error
		if request.URL.Query().Get("scrollback") == "1" && cols == 0 {
			text, seq, err = session.TerminalSnapshot(request.Context())
		} else {
			text, seq, err = session.Snapshot(request.Context(), cols)
		}
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		response.Header().Set("Content-Type", "text/plain; charset=utf-8")
		setCORSHeaders(response, corsOrigin)
		if corsOrigin != "" {
			response.Header().Set("Access-Control-Expose-Headers", "X-Sessions-Seq")
		}
		response.Header().Set("X-Sessions-Seq", strconv.FormatUint(uint64(seq), 10))
		response.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(response, text)
		return
	}
	if suffix == "/events" && request.Method == http.MethodGet {
		if !ok {
			s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "unknown session", "id": id}, corsOrigin)
			return
		}
		since := queryIndex(request.URL.Query(), "since")
		tail := queryIndex(request.URL.Query(), "tail")
		before := queryIndex(request.URL.Query(), "before")
		s.sendJSON(response, http.StatusOK, s.eventsWindowBody(request.Context(), session, id, since, tail, before), corsOrigin)
		return
	}
	if (suffix == "/input" || suffix == "/submit") && request.Method == http.MethodPost {
		var body struct {
			Data        string `json:"data"`
			OperationID string `json:"operation_id,omitempty"`
		}
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		attribution, attributed, err := captureInputAttribution(request)
		if err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		if suffix == "/submit" {
			// Keyed by session: the text and its Enter stay adjacent for THIS
			// session while submits to other sessions run at the same time.
			defer s.submits.lock(id)()
			if body.OperationID == "" {
				body.OperationID, err = delivery.NewOperationID()
				if err != nil {
					s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": "create delivery operation: " + err.Error()}, corsOrigin)
					return
				}
			}
			record, created, beginErr := s.deliveries.Begin(body.OperationID, id, body.Data)
			if beginErr != nil {
				s.sendJSON(response, http.StatusConflict, map[string]any{"error": beginErr.Error(), "operation_id": body.OperationID}, corsOrigin)
				return
			}
			if !created {
				s.sendDeliveryRecord(response, record, true, corsOrigin)
				return
			}
		}
		if !ok {
			if suffix == "/submit" {
				record, completeErr := s.deliveries.Complete(body.OperationID, delivery.StatusNotDelivered, false, true, "unknown session")
				if completeErr == nil {
					s.sendDeliveryRecord(response, record, false, corsOrigin)
					return
				}
			}
			s.sendJSON(response, http.StatusNotFound, map[string]any{"ok": false}, corsOrigin)
			return
		}
		if err := s.writeSessionInput(request.Context(), id, body.Data, attribution, attributed); err != nil {
			if suffix == "/submit" {
				record, completeErr := s.deliveries.Complete(body.OperationID, delivery.StatusUnknown, false, false, err.Error())
				if completeErr == nil {
					s.sendDeliveryRecord(response, record, false, corsOrigin)
					return
				}
			}
			var unavailable *sessionruntime.MessageInputUnavailableError
			if !attributed && errors.As(err, &unavailable) {
				s.sendJSON(response, http.StatusNotFound, map[string]any{"ok": false}, corsOrigin)
				return
			}
			s.sendInputError(response, err, corsOrigin)
			return
		}
		if suffix == "/submit" {
			timer := time.NewTimer(submitSettleDelay)
			select {
			case <-request.Context().Done():
				timer.Stop()
				record, completeErr := s.deliveries.Complete(body.OperationID, delivery.StatusTextOnly, true, false, request.Context().Err().Error())
				if completeErr != nil {
					s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": completeErr.Error(), "operation_id": body.OperationID, "delivered": true, "retry": false}, corsOrigin)
					return
				}
				s.sendDeliveryRecord(response, record, false, corsOrigin)
				return
			case <-timer.C:
			}
			if !s.registry.Input(request.Context(), id, "\r") {
				record, completeErr := s.deliveries.Complete(body.OperationID, delivery.StatusTextOnly, true, false, "message text was delivered but Enter could not be sent")
				if completeErr != nil {
					s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": completeErr.Error(), "operation_id": body.OperationID, "delivered": true, "retry": false}, corsOrigin)
					return
				}
				s.sendDeliveryRecord(response, record, false, corsOrigin)
				return
			}
			record, completeErr := s.deliveries.Complete(body.OperationID, delivery.StatusAccepted, true, false, "")
			if completeErr != nil {
				s.sendJSON(response, http.StatusInternalServerError, map[string]any{
					"error":        "message was accepted but its durable receipt failed: " + completeErr.Error(),
					"operation_id": body.OperationID, "delivered": true, "retry": false,
				}, corsOrigin)
				return
			}
			s.sendDeliveryRecord(response, record, false, corsOrigin)
			return
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"ok": true, "attributed": attributed}, corsOrigin)
		return
	}
	if suffix == "/upload" && request.Method == http.MethodPost {
		if !ok {
			s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "unknown session", "id": id}, corsOrigin)
			return
		}
		s.handleUpload(response, request, corsOrigin)
		return
	}
	s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "not found", "path": request.URL.Path}, corsOrigin)
}

func sessionRuntimeRoute(method, suffix string) bool {
	switch suffix {
	case "/snapshot", "/events", "/model-options":
		return method == http.MethodGet
	case "/input", "/submit":
		return method == http.MethodPost
	case "/model":
		return method == http.MethodPut
	default:
		return false
	}
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

// setCORSHeaders writes the one CORS answer this daemon gives. Every response
// helper calls it, so a client cannot learn a different allowed-method or
// allowed-header set from one endpoint than from another.
