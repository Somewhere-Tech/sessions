package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/somewhere-tech/sessions/runtime/internal/backup"
	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/recap"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/smartsearch"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/usage"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
	"github.com/somewhere-tech/sessions/runtime/internal/webassets"
)

const (
	maxJSONBody          = 2 * 1024 * 1024
	creatorSessionHeader = "X-Sessions-Creator-Session"
	creatorOwnerHeader   = "X-Sessions-Owner-ID"
	endClientHeader      = "X-Sessions-Client"
	apiProtocolVersion   = 1
	minimumAPIClient     = 1
	maximumAPIClient     = apiProtocolVersion
)

// Version is stamped into sessionsd at build time and reported by both health
// endpoints. Keep the source fallback aligned with the current app version so
// an un-stamped development build is still honest.
var Version = "0.2.13"

type Server struct {
	config               state.Config
	registry             sessionService
	push                 pushService
	tokens               tokenStore
	pair                 *pairService
	tailnetAccess        *tailnetAccessService
	lan                  *lanListener
	backups              *backup.Service
	integrationEndpoints *integrations.Service
	usage                *usage.Service
	recaps               *recap.Service
	smartSearch          *smartsearch.Service
	identity             machineIdentity
	identityError        error
}

type authPrincipal struct {
	Local bool
	Kind  ledger.CreatorKind
	ID    string
	Name  string
}

type authPrincipalContextKey struct{}

type sessionService interface {
	Uptime() time.Duration
	IsDiscovering() bool
	Create(context.Context, state.CreateSessionRequest) (state.SessionInfo, error)
	List(bool) []state.SessionInfo
	Get(string) (*state.Session, bool)
	Tags(string) (map[string]string, error)
	UpdateTags(string, map[string]string) (map[string]string, error)
	RequestKill(context.Context, string, bool) error
	Input(context.Context, string, string) bool
	DeepDiagnostics() []map[string]any
}

type attributedKillService interface {
	RequestKillAttributed(context.Context, string, bool, state.EndSessionRequest) error
}

type attributedBatchKillService interface {
	KillManyAttributed(context.Context, []string, bool, state.EndSessionRequest) error
}

type attributedInputService interface {
	InputAttributed(context.Context, string, string, state.InputAttribution) error
}

type messageAttributionService interface {
	MessageRelays(context.Context, string) ([]ledger.MessageRelayed, error)
}

type modelControlService interface {
	ConfigureModel(context.Context, string, string, string) (state.SessionInfo, error)
}

type modelCatalogService interface {
	ModelOptions(context.Context, string) ([]codexapp.Model, error)
}

type newSessionModelCatalogService interface {
	CodexModelOptions(context.Context) ([]codexapp.Model, error)
}

type pushService interface {
	VAPIDPublicKey() (string, error)
	AddSubscription(any) error
	RemoveSubscription(string) error
}

func New(config state.Config, registry sessionService, pushes ...pushService) *Server {
	return NewWithUsage(config, registry, nil, pushes...)
}

func NewWithUsage(config state.Config, registry sessionService, localUsage *usage.Service, pushes ...pushService) *Server {
	var notifications pushService
	if len(pushes) > 0 {
		notifications = pushes[0]
	} else {
		root := config.UserStateRoot
		if root == "" {
			root = config.StateRoot
		}
		notifications = sessionruntime.NewPushService(root)
	}
	identity, identityErr := loadOrCreateMachineIdentity(config)
	server := &Server{
		config: config, registry: registry, push: notifications, tokens: tokenStore{path: config.TokenPath},
		pair:          newPairService(config),
		tailnetAccess: newTailnetAccessService(),
		identity:      identity, identityError: identityErr,
		integrationEndpoints: integrations.NewService(integrations.ServiceOptions{
			StateDir: config.StateRoot, RunnerStateDir: config.RunnerStateDir,
			DiscoverProviderHistory: true,
		}),
	}
	if localUsage == nil {
		localUsage = usage.NewLocalService(config)
	}
	server.usage = localUsage
	recapRoot := config.StateRoot
	if recapRoot == "" {
		recapRoot = config.UserStateRoot
	}
	server.recaps = recap.NewService(recapRoot)
	server.smartSearch = smartsearch.NewService()
	server.lan = newLANListener(config, server, identity)
	// Create the token while the daemon is starting, including when the open
	// escape hatch is present. This keeps a fresh install secure without an
	// inbound request and makes `sessions token` immediately useful. A failure
	// remains fail-closed: non-loopback authorization retries and returns 500.
	_, _ = server.tokens.token()
	if home, ok := backupHome(config.UserStateRoot); ok {
		server.backups = backup.NewService(backup.Options{
			ConfigPath: backup.ConfigPath(home), RunnerStateDir: config.RunnerStateDir,
		}, func() []state.SessionInfo { return registry.List(true) })
		_ = server.backups.ReloadPeriodic()
	}
	return server
}

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
		s.sendJSON(response, http.StatusOK, map[string]any{
			"ok": true, "name": "sessionsd", "version": Version,
			"listen": map[string]any{"host": s.config.Host, "port": s.config.Port},
			"lan":    s.lan.state(),
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
		}, corsOrigin)
		return
	}
	if path == "/api/health/deep" && request.Method == http.MethodGet {
		s.sendJSON(response, http.StatusOK, map[string]any{
			"ok": true, "name": "sessionsd", "version": Version,
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
			"uptimeSec":      int64(math.Round(s.registry.Uptime().Seconds())),
			"sessions":       s.registry.DeepDiagnostics(),
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
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	if !authorized {
		s.sendJSON(response, http.StatusUnauthorized, map[string]any{"error": "unauthorized"}, corsOrigin)
		return
	}
	request = request.WithContext(context.WithValue(request.Context(), authPrincipalContextKey{}, principal))
	if path == "/ws" {
		if !allowedOrigin(origin, s.config.Host, s.lan.activeHost()) {
			s.sendJSON(response, http.StatusForbidden, map[string]any{"error": "forbidden origin"}, "")
			return
		}
		s.serveWebSocket(response, request)
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
	if path == "/api/recovery" || path == "/api/recovery/reopen" || path == "/api/recovery/adopt" {
		s.handleRecovery(response, request, corsOrigin)
		return
	}
	if path == "/api/directories" && request.Method == http.MethodGet {
		s.sendJSON(response, http.StatusOK, map[string]any{"directories": listDirectoryCandidates()}, corsOrigin)
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
		sessions, err := s.resumableConversations()
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"sessions": sessions}, corsOrigin)
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
		s.handleSessionRoute(response, request, id, suffix, corsOrigin)
		return
	}
	s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "not found", "path": path}, corsOrigin)
}

func (s *Server) authorized(request *http.Request) (authPrincipal, bool, error) {
	localUserID, err := ledger.LocalUserCreatorID()
	if err != nil {
		return authPrincipal{}, false, err
	}
	localUser := authPrincipal{
		Local: true,
		Kind:  ledger.CreatorUser,
		ID:    localUserID,
		Name:  "Local user",
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
		return authPrincipal{Kind: ledger.CreatorExternal, ID: "remote:master-token", Name: "Remote administrator"}, true, nil
	}
	if strings.HasPrefix(authorization, "Bearer ") {
		if device, authorized, err := s.pair.devices.authenticate(strings.TrimPrefix(authorization, "Bearer ")); authorized || err != nil {
			if err != nil {
				return authPrincipal{}, false, err
			}
			return authPrincipal{
				Kind: ledger.CreatorExternal, ID: "device:" + device.DeviceID, Name: device.Name,
			}, true, nil
		}
	}
	provided := request.URL.Query().Get("token")
	if provided == "" {
		return authPrincipal{}, false, nil
	}
	if tokenEqual(provided, expected) {
		return authPrincipal{Kind: ledger.CreatorExternal, ID: "remote:master-token", Name: "Remote administrator"}, true, nil
	}
	device, authorized, err := s.pair.devices.authenticate(provided)
	if err != nil || !authorized {
		return authPrincipal{}, authorized, err
	}
	return authPrincipal{
		Kind: ledger.CreatorExternal, ID: "device:" + device.DeviceID, Name: device.Name,
	}, true, nil
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
		}
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
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
			s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "unknown session", "id": id}, corsOrigin)
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
		text, seq, err := session.Snapshot(request.Context(), cols)
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		response.Header().Set("Content-Type", "text/plain; charset=utf-8")
		response.Header().Set("Vary", "Origin")
		if corsOrigin != "" {
			response.Header().Set("Access-Control-Allow-Origin", corsOrigin)
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
		window := session.EventsWindow(since, tail, before)
		events := window.Events
		full := session.EventsWindow(nil, nil, nil)
		if annotated, err := s.annotateRawEvents(request.Context(), id, full.Events); err != nil {
			log.Printf("[attribution] annotate live events for %s: %v", id, err)
		} else {
			start := window.StartIndex - full.StartIndex
			end := window.EndIndex - full.StartIndex
			if start >= 0 && end >= start && end <= int64(len(annotated)) {
				events = annotated[start:end]
			}
		}
		s.sendJSON(response, http.StatusOK, map[string]any{
			"events": events, "nextIndex": window.NextIndex, "totalCount": window.TotalCount,
			"startIndex": window.StartIndex, "endIndex": window.EndIndex,
		}, corsOrigin)
		return
	}
	if suffix == "/input" && request.Method == http.MethodPost {
		var body struct {
			Data string `json:"data"`
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
		if attributed {
			service, supported := s.registry.(attributedInputService)
			if !supported {
				s.sendJSON(response, http.StatusNotImplemented, map[string]any{"error": "message attribution is unavailable"}, corsOrigin)
				return
			}
			err := service.InputAttributed(request.Context(), id, body.Data, attribution)
			if err != nil {
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
					s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
				}
				return
			}
			s.sendJSON(response, http.StatusOK, map[string]any{"ok": true, "attributed": true}, corsOrigin)
			return
		}
		result := ok && s.registry.Input(request.Context(), id, body.Data)
		if !result {
			s.sendJSON(response, http.StatusNotFound, map[string]any{"ok": false}, corsOrigin)
			return
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"ok": true}, corsOrigin)
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

func (s *Server) sendJSON(response http.ResponseWriter, status int, body any, corsOrigin string) {
	response.Header().Set("Content-Type", "application/json")
	if corsOrigin != "" {
		response.Header().Set("Access-Control-Allow-Origin", corsOrigin)
	}
	response.Header().Set("Vary", "Origin")
	response.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
	response.Header().Set("Access-Control-Allow-Headers", "content-type, authorization, x-sessions-creator-session, x-sessions-owner-id, x-sessions-client, x-sessions-filename")
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

func readJSON(request *http.Request, target any) error {
	if request.ContentLength != 0 {
		contentTypes := request.Header.Values("Content-Type")
		if len(contentTypes) != 1 {
			return errors.New("content-type must be application/json")
		}
		mediaType, _, err := mime.ParseMediaType(contentTypes[0])
		if err != nil || mediaType != "application/json" {
			return errors.New("content-type must be application/json")
		}
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
