package api

import (
	"context"
	"path/filepath"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/backup"
	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
	"github.com/somewhere-tech/sessions/runtime/internal/delivery"
	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/project"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/smartsearch"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/usage"
)

const (
	maxJSONBody          = 2 * 1024 * 1024
	creatorSessionHeader = "X-Sessions-Creator-Session"
	creatorOwnerHeader   = "X-Sessions-Owner-ID"
	endClientHeader      = "X-Sessions-Client"
	apiProtocolVersion   = 1
	minimumAPIClient     = 1
	maximumAPIClient     = apiProtocolVersion
	submitSettleDelay    = 150 * time.Millisecond
)

// Version is stamped into sessionsd at build time and reported by both health
// endpoints. Keep the source fallback aligned with the current app version so
// an un-stamped development build is still honest.
var Version = "0.2.27"

type Server struct {
	config               state.Config
	projects             *project.Store
	registry             sessionService
	push                 pushService
	tokens               tokenStore
	pair                 *pairService
	tailnetAccess        *tailnetAccessService
	lan                  *lanListener
	backups              *backup.Service
	integrationEndpoints *integrations.Service
	usage                *usage.Service
	smartSearch          *smartsearch.Service
	deliveries           *delivery.Store
	identity             machineIdentity
	identityError        error
	submits              *sessionMutexes
}

type authPrincipal struct {
	Local     bool
	HostAdmin bool
	Kind      ledger.CreatorKind
	ID        string
	Name      string
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

type rebootRestoreHealthService interface {
	RestorePendingCount() int
}

type pendingRestoreService interface {
	PendingRestore(string) (state.RestorePending, bool)
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

type pausedWaker interface {
	WakePaused(context.Context, string) (state.SessionInfo, error)
}

type approvalService interface {
	Approve(context.Context, string, proto.ApprovalControl) (state.SessionInfo, error)
}

type modelControlService interface {
	ConfigureModel(context.Context, string, string, string) (state.SessionInfo, error)
}

type providerRetryService interface {
	RetryProvider(context.Context, string) (state.SessionInfo, error)
	StopProviderRetry(context.Context, string) error
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
	deliveryRoot := config.StateRoot
	if deliveryRoot == "" {
		deliveryRoot = config.UserStateRoot
	}
	server := &Server{
		config: config, registry: registry, push: notifications, tokens: tokenStore{path: config.TokenPath},
		pair:          newPairService(config),
		tailnetAccess: newTailnetAccessService(),
		submits:       newSessionMutexes(),
		identity:      identity, identityError: identityErr,
		deliveries: delivery.New(deliveryRoot),
		integrationEndpoints: integrations.NewService(integrations.ServiceOptions{
			StateDir: config.StateRoot, RunnerStateDir: config.RunnerStateDir,
			DiscoverProviderHistory: true,
		}),
	}
	if localUsage == nil {
		localUsage = usage.NewLocalService(config)
	}
	server.usage = localUsage
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
	// Projects live beside settings in the user state root; a daemon with only
	// a state root (tests, isolated runs) keeps them there rather than in the
	// working directory.
	projectsRoot := config.UserStateRoot
	if projectsRoot == "" {
		projectsRoot = config.StateRoot
	}
	if projectsRoot != "" {
		server.projects = project.NewStore(filepath.Join(projectsRoot, "projects.json"), nil)
	}
	return server
}
