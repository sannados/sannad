package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/getkayan/kayan/core/tenant"
	"github.com/glebarez/sqlite"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/adaptor"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/limiter"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/gofiber/fiber/v2/middleware/requestid"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	kernelV1 "github.com/sannados/sannad/contracts/gen/go/sannad/kernel/v1"
	"github.com/sannados/sannad/internal/app/migrations"
	"github.com/sannados/sannad/internal/kernel/bridge"
	"github.com/sannados/sannad/internal/kernel/bus"
	"github.com/sannados/sannad/internal/kernel/events"
	"github.com/sannados/sannad/internal/kernel/registry"
	"github.com/sannados/sannad/internal/kernel/wasmhost"
	"github.com/sannados/sannad/internal/platform/auth"
	"github.com/sannados/sannad/internal/platform/storage"
	"github.com/sannados/sannad/internal/platform/telemetry"
	"github.com/sannados/sannad/internal/platform/tenancy"
	"github.com/sannados/sannad/internal/platform/webhooks"
	"github.com/sannados/sannad/pkg/config"
	"github.com/sannados/sannad/pkg/modulekit"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type App struct {
	Config       config.Config
	Registry     *registry.Registry
	Bus          *bus.Bus
	Auth         *auth.Service
	Webhooks     *webhooks.Runner
	db           *gorm.DB
	store        modulekit.Store
	events       events.Publisher
	telemetry    *telemetry.Provider
	kernelFiber  *fiber.App
	gatewayFiber *fiber.App
	// tenantLimiter throttles authenticated requests per tenant+caller,
	// independent of the global IP-keyed limiter below. The IP limiter alone
	// lets one noisy tenant, or one compromised service account, exhaust the
	// budget every other tenant shares behind the same NAT/proxy IP; keying
	// by tenant+subject instead means a limit hit stays contained to the
	// account that caused it. Built once per gatewayHandler call so its
	// counters are shared across every route rather than reset per request.
	tenantLimiter fiber.Handler
	grpcServer    *grpc.Server
	ready         atomic.Bool
	// webhookSub is the durable event-stream subscription queuing deliveries.
	// nil when the deployment has no event broker configured — the noop
	// publisher does not implement events.Consumer, so there is nothing to
	// subscribe to.
	webhookSub events.Subscription
	// webhookCancel stops the drain loop's ticker goroutine at Shutdown.
	webhookCancel context.CancelFunc
	// webhookDone is closed when the drain loop goroutine returns, so Shutdown
	// can wait for it rather than leaving it running past the app's lifetime.
	webhookDone chan struct{}
	// grpcServices are entrypoint-supplied gRPC service registrations, applied
	// when the server is built. The kernel names no domain service itself.
	grpcServices []func(*grpc.Server)
	// httpRoutes are entrypoint-supplied gateway routes, applied when the
	// gateway is built. Same rule as grpcServices: domain endpoints are not
	// the kernel's to name.
	httpRoutes []func(fiber.Router, fiber.Handler)
	// tenants is the directory of provisioned tenants. Unauthenticated requests
	// are checked against it, so an identifier nobody provisioned is refused
	// before a login flow runs rather than silently becoming a working tenant.
	tenants *tenancy.Directory
	// tenantResolver resolves the tenant for unauthenticated requests from the
	// host. nil when no base domain is configured, in which case those requests
	// name their tenant explicitly. Authenticated requests never use it.
	tenantResolver tenant.Resolver
}

type credentialsPayload struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	// Tenant identifies the tenant being registered into or logged into. It is
	// used only when the host does not already identify one (see resolveTenant),
	// and is safe here because these endpoints are unauthenticated: there is no
	// established session for it to escalate against. Every authenticated
	// request takes its tenant from the signed token instead.
	Tenant string `json:"tenant,omitempty"`
}

func New(cfg config.Config) (*App, error) {
	database, err := openDatabase(cfg.DatabaseDSN, cfg)
	if err != nil {
		return nil, err
	}

	sqlDB, err := database.DB()
	if err != nil {
		return nil, fmt.Errorf("bootstrap: get sql.DB: %w", err)
	}
	if err := migrations.RunMigrations(sqlDB, cfg.DatabaseDSN); err != nil {
		return nil, fmt.Errorf("bootstrap: run migrations: %w", err)
	}

	// Tenant isolation is installed once, here, before any module is
	// constructed. Every module receives this same *gorm.DB, so there is no
	// unhooked path to the database and no per-query opt-in to forget.
	// ADR 0005.
	if err := tenancy.RegisterIsolation(database); err != nil {
		return nil, fmt.Errorf("bootstrap: register tenant isolation: %w", err)
	}
	// Record which tables are scoped, so that queries addressing a table by name
	// rather than by model — Table("contacts"), raw SQL — are still recognised.
	// Every scoped model in the deployment belongs in this list.
	//
	// The kernel owns no domain models; an entrypoint registers its modules'
	// models via App.RegisterScopedModels. It does own the SDK's own tables,
	// though, and they are tenant-scoped like any other. Registering them by
	// model is what makes a by-name or raw-SQL query against them refused
	// instead of silently spanning tenants — the type-based check already covers
	// queries that pass the model, so this closes the path that does not.
	//
	// The startup audit in Start reports any scoped-looking table missing from
	// this list; these four were exactly what it found on its first run.
	if err := tenancy.RegisterScopedModels(database,
		&modulekit.ProcessedEvent{},
		&modulekit.ProjectionState{},
		&modulekit.SagaRecord{},
		&modulekit.SagaStepRecord{},
		&webhooks.Endpoint{},
		&webhooks.Delivery{},
	); err != nil {
		return nil, fmt.Errorf("bootstrap: register SDK scoped models: %w", err)
	}

	authService, err := auth.NewService(database, cfg.SessionSecret, cfg.SessionPreviousSecrets...)
	if err != nil {
		return nil, err
	}
	if cfg.OIDCProvidersJSON != "" {
		providers, err := parseOIDCProviders(cfg.OIDCProvidersJSON)
		if err != nil {
			return nil, fmt.Errorf("bootstrap: parse OIDC providers: %w", err)
		}
		if err := authService.InitOIDC(context.Background(), providers); err != nil {
			return nil, err
		}
	}
	if cfg.SAMLProvidersJSON != "" {
		providers, err := parseSAMLProviders(cfg.SAMLProvidersJSON)
		if err != nil {
			return nil, fmt.Errorf("bootstrap: parse SAML providers: %w", err)
		}
		spConfig := auth.SAMLSPConfig{EntityID: cfg.SAMLEntityID, ACSUrl: cfg.SAMLACSUrl}
		if err := authService.InitSAML(context.Background(), spConfig, providers); err != nil {
			return nil, fmt.Errorf("bootstrap: init SAML: %w", err)
		}
	}

	app := &App{
		Config:   cfg,
		Registry: registry.New(),
		Auth:     authService,
		db:       database,
		// Constructed from the connection that already carries the isolation
		// callbacks, so every module query is scoped by construction.
		store: storage.NewGormStore(database),
	}
	publisher, err := buildPublisher(cfg.NATSUrl)
	if err != nil {
		return nil, err
	}
	app.events = publisher
	tp, err := telemetry.Setup(context.Background(), "sannad-kernel", cfg.OTELEndpoint)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: init telemetry: %w", err)
	}
	app.telemetry = tp
	app.tenants = tenancy.NewDirectory(database)
	if cfg.TenantBaseDomain != "" {
		// Wrapped rather than replaced: the resolution strategy and the question
		// of whether the result is usable are separate concerns.
		app.tenantResolver = tenancy.NewValidatingResolver(
			tenant.NewSubdomainResolver(cfg.TenantBaseDomain), app.tenants)
	}
	app.Bus = bus.New(app.Registry)
	app.Webhooks = webhooks.NewRunner(app.store)
	// Servers are built in Start, not here: entrypoints register their routes
	// and gRPC services after New returns, and anything registered after the
	// server was built would be silently dropped.
	return app, nil
}

// buildPublisher returns a NATS publisher when a URL is provided, or a no-op
// publisher that silently discards events (for dev and test environments).
// buildPublisher returns the event publisher for a deployment.
//
// No URL means events are deliberately disabled and a no-op publisher is
// correct: local development and tests run without a broker.
//
// A URL that fails to connect is a different case, and used to be treated the
// same — logged as a warning and silently replaced with the no-op, so a
// misconfigured broker meant every event vanished while the system reported
// healthy. A deployment that configured NATS asked for events; failing to start
// is better than running without them and discovering months later that nothing
// was ever delivered.
func buildPublisher(natsURL string) (events.Publisher, error) {
	if natsURL == "" {
		slog.Info("events: no broker configured, events are disabled")
		return events.NoopPublisher{}, nil
	}
	p, err := events.NewNATSPublisher(natsURL)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: connect to NATS at %s: %w", natsURL, err)
	}
	slog.Info("events: connected to NATS", "url", natsURL)
	return p, nil
}

// Store returns the storage handle modules are constructed with.
//
// This deliberately returns modulekit.Store rather than *gorm.DB: an entrypoint
// that could reach the engine type would let module constructors take one, which
// is the coupling ADR 0006 removes. The connection itself stays private to this
// package.
func (app *App) Store() modulekit.Store {
	return app.store
}

// EventPublisher returns the application-level event publisher. Use this in
// cmd/ entrypoints to wire the publisher into modules via WithPublisher.
func (app *App) EventPublisher() modulekit.EventPublisher {
	return app.events
}

// RegisterScopedModels subjects a module's models to tenant isolation.
//
// The kernel names no domain model, so an entrypoint must declare its modules'
// tenant-scoped models here. A model that is never registered is silently
// unisolated, which is why this is called at wiring time and fails loudly.
func (app *App) RegisterScopedModels(models ...tenant.TenantAware) error {
	if app.db == nil {
		return errors.New("bootstrap: database not initialised")
	}
	return tenancy.RegisterScopedModels(app.db, models...)
}

// RegisterGRPCService records a gRPC service registration to be applied when
// the gRPC server is built. A gRPC service is a projection of one module's
// contract, so the kernel does not name it; entrypoints wire it in.
//
// Must be called before Start.
func (app *App) RegisterGRPCService(register func(*grpc.Server)) {
	app.grpcServices = append(app.grpcServices, register)
}

// RegisterAuthenticatedRoutes records gateway routes to be mounted when the
// gateway is built. The callback receives the router and the auth middleware,
// so a module's endpoints sit behind the same requireAuth the kernel's own
// routes use — the tenant reaches storage from the signed token either way.
//
// Must be called before Start.
func (app *App) RegisterAuthenticatedRoutes(mount func(router fiber.Router, requireAuth fiber.Handler)) {
	app.httpRoutes = append(app.httpRoutes, mount)
}

// RegisterModule installs a module into the kernel registry. Must be called
// before Start; returns an error if the application is already running.
func (app *App) RegisterModule(m modulekit.Module) error {
	if app.ready.Load() {
		return errors.New("bootstrap: cannot register modules after the application has started")
	}

	// A module owns the declaration of its tenant-scoped models. Discovering it
	// here keeps registration atomic from the application author's perspective:
	// importing a module and then forgetting a second isolation call is no longer
	// a valid wiring state. The manual method remains for older modules.
	if provider, ok := m.(modulekit.ScopedModelProvider); ok {
		models := provider.ScopedModels()
		for i, model := range models {
			if model == nil {
				return fmt.Errorf("bootstrap: module %s declares nil scoped model at index %d",
					m.Descriptor().ID, i)
			}
		}
		kayanModels := make([]tenant.TenantAware, len(models))
		for i, model := range models {
			kayanModels[i] = model
		}
		if err := app.RegisterScopedModels(kayanModels...); err != nil {
			return fmt.Errorf("bootstrap: register scoped models for %s: %w",
				m.Descriptor().ID, err)
		}
	}

	// Schema before Install, because Install registers capabilities and a module
	// may query its own tables during Start. Migrating afterwards would leave a
	// window where the module is callable and its tables do not exist.
	//
	// A failure here stops registration: a module whose schema did not apply
	// would fail on its first query, at a point far from the cause.
	if app.db != nil {
		sqlDB, err := app.db.DB()
		if err != nil {
			return fmt.Errorf("bootstrap: get sql.DB for module migrations: %w", err)
		}
		if err := migrations.RunModuleMigrations(
			context.Background(), sqlDB, app.Config.DatabaseDSN, m); err != nil {
			return err
		}
	}

	return app.Registry.RegisterModule(m)
}

// RegisterWASM loads operator-approved guest bytes and registers them through
// the ordinary module lifecycle. The guest does not supply its own grants or
// publication surface: those come from config approved by the deployment.
func (app *App) RegisterWASM(ctx context.Context, config wasmhost.ModuleConfig) error {
	// The guest never receives these directly, and a caller registering a WASM
	// module has no business constructing its own: the manifest names what a
	// module may consume or publish, and the platform's own bus and event
	// publisher are what those grants resolve through. Filled in only when the
	// caller left them nil, so a test can still substitute its own.
	if config.Caller == nil {
		config.Caller = app.Bus
	}
	if config.Events == nil {
		config.Events = app.events
	}

	module, err := wasmhost.NewModule(ctx, config)
	if err != nil {
		return err
	}
	if err := app.RegisterModule(module); err != nil {
		return errors.Join(err, module.Close(ctx))
	}
	return nil
}

func (app *App) RunGRPC(_ context.Context) error {
	if app.Config.GRPCAddr == "" {
		return nil
	}
	lis, err := net.Listen("tcp", app.Config.GRPCAddr)
	if err != nil {
		return err
	}
	slog.Info("sannad grpc bridge listening", "addr", app.Config.GRPCAddr)
	return app.grpcServer.Serve(lis)
}

func (app *App) RunKernel(ctx context.Context) error {
	_ = ctx
	slog.Info("sannad kernel listening", "addr", app.Config.KernelAddr)
	return app.kernelFiber.Listen(app.Config.KernelAddr)
}

func (app *App) RunGateway(ctx context.Context) error {
	_ = ctx
	slog.Info("sannad gateway listening", "addr", app.Config.GatewayAddr)
	return app.gatewayFiber.Listen(app.Config.GatewayAddr)
}

// TestGateway dispatches req directly into the gateway Fiber app without
// opening a real TCP connection. Intended for use in tests only.
func (app *App) TestGateway(req *http.Request) (*http.Response, error) {
	return app.gatewayFiber.Test(req, -1)
}

// TestGRPCServer returns the built gRPC server so a test can serve it over an
// in-memory listener. Intended for use in tests only; nil before Start.
//
// Exposed because the interceptor is the part worth testing: it is registered
// server-wide, so a test that constructed its own server would verify dispatch
// while skipping authentication entirely.
func (app *App) TestGRPCServer() *grpc.Server {
	return app.grpcServer
}

// fiberConfig returns a shared Fiber configuration with production-safe timeouts,
// body size limits, and a JSON-only error handler.
func (app *App) fiberConfig() fiber.Config {
	return fiber.Config{
		DisableStartupMessage: true,
		ReadTimeout:           time.Duration(app.Config.ReadTimeout) * time.Second,
		WriteTimeout:          time.Duration(app.Config.WriteTimeout) * time.Second,
		IdleTimeout:           time.Duration(app.Config.IdleTimeout) * time.Second,
		BodyLimit:             app.Config.BodyLimit,
		ErrorHandler:          jsonErrorHandler,
	}
}

// auditExemptTables are tables that carry a tenant column and deliberately are
// not tenant-scoped. Each needs a reason, because an exemption without one is
// indistinguishable from the mistake the audit exists to catch.
//
//   - accounts: login looks an account up by email before any session, and
//     therefore any tenant context, exists. A scoped Account would make that
//     lookup fail closed and nobody could log in. Isolation for accounts is
//     enforced explicitly in the auth service, which is the one place it can be.
//     See the comment on auth.Account and ADR 0005.
//   - service_accounts: the identical reason. Authenticating a service account
//     by API key means looking its row up by key hash before any tenant
//     context exists. Isolation is enforced explicitly in auth.Service's
//     service-account methods. See the comment on auth.ServiceAccount.
//   - role_assignments, role_definitions: tried as isolation-callback-scoped
//     models first. A FirstOrCreate through the callback produced a row with
//     an empty tenant_id — traced to how that GORM call shape interacts with
//     the callback, not fixed further. Every rbacRepo method now filters and
//     stamps tenant_id explicitly instead. See the comment at the top of
//     internal/platform/auth/rbac.go.
//   - audit_events: written through gormstore.Repository, which implements
//     audit.AuditStore directly — every write and query already carries an
//     explicit TenantID (EventBuilder.Tenant, Filter.TenantID), the same
//     explicit-scoping pattern as the three tables above, not an implicit
//     database-layer one.
var auditExemptTables = []string{"accounts", "service_accounts", "role_assignments", "role_definitions", "audit_events"}

func (app *App) Start(ctx context.Context) error {
	if err := app.Registry.StartAll(ctx); err != nil {
		return err
	}

	// After StartAll, so every module has registered its scoped models. Running
	// this earlier would report modules that were simply not installed yet.
	//
	// Findings are logged, not fatal. A table with a tenant column may be
	// legitimately global, and refusing to boot would mean an operator upgrading
	// the kernel is locked out by a module they did not write — which makes
	// running the audit at all the risky move. An audit failure is likewise not
	// fatal: an unreadable schema is a reason to lose the check, not the process.
	if findings, err := tenancy.AuditScopedModels(ctx, app.db, auditExemptTables...); err != nil {
		slog.WarnContext(ctx, "tenancy audit did not run", "err", err)
	} else {
		tenancy.LogAuditFindings(ctx, findings)
	}
	// Reported alongside the audit, because it is the same question: what is the
	// isolation surface an operator is trusting. The audit covers what it can
	// see; this names what it cannot.
	tenancy.LogSelfManagedStorage(ctx, app.Registry.SelfManagedStorage())

	// Deliveries were queued and drained by nothing before this: the runner and
	// dispatcher existed, wired to no event source and no schedule. A consumer
	// is only available when a real broker is configured — events.NoopPublisher
	// does not implement events.Consumer — so a deployment with no NATS URL
	// runs with webhooks registrable but never fired, logged rather than failed
	// closed, the same choice made for a missing broker generally.
	webhookCtx, cancel := context.WithCancel(context.Background())
	app.webhookCancel = cancel
	app.webhookDone = make(chan struct{})
	if consumer, ok := app.events.(events.Consumer); ok {
		sub, err := app.Webhooks.StartConsuming(webhookCtx, consumer)
		if err != nil {
			cancel()
			close(app.webhookDone)
			return fmt.Errorf("bootstrap: start webhook consumer: %w", err)
		}
		app.webhookSub = sub
		go func() {
			defer close(app.webhookDone)
			app.Webhooks.RunDrainLoop(webhookCtx, webhooks.DefaultDrainInterval, webhooks.DefaultDrainBatchSize)
		}()
	} else {
		slog.WarnContext(ctx, "webhooks: no event consumer available, deliveries will not be queued or drained")
		close(app.webhookDone)
	}

	// Built here so entrypoint-registered routes and services are included.
	app.grpcServer = app.buildGRPCServer()
	app.kernelFiber = app.kernelHandler()
	app.gatewayFiber = app.gatewayHandler()
	app.ready.Store(true)
	return nil
}

func (app *App) Shutdown(ctx context.Context) error {
	app.ready.Store(false)
	// Servers are built in Start, so Shutdown on a New-but-never-Started app
	// finds them nil. That happens on any wiring error between the two, which
	// is exactly when cleanup still has to release the database pool.
	if app.grpcServer != nil {
		app.grpcServer.GracefulStop()
	}

	// Stop the drain loop and the durable subscription before the database and
	// broker connections they depend on go away. Draining position — how far
	// the durable consumer has read — survives on the broker side; only this
	// process's local loop needs to be told to stop.
	if app.webhookCancel != nil {
		app.webhookCancel()
	}
	if app.webhookDone != nil {
		<-app.webhookDone
	}
	var webhookSubErr error
	if app.webhookSub != nil {
		webhookSubErr = app.webhookSub.Close()
	}

	eventsErr := app.events.Close()
	telemetryErr := app.telemetry.Shutdown(ctx)
	stopErr := app.Registry.StopAll(ctx)
	var kernelErr, gatewayErr error
	if app.kernelFiber != nil {
		kernelErr = app.kernelFiber.ShutdownWithContext(ctx)
	}
	if app.gatewayFiber != nil {
		gatewayErr = app.gatewayFiber.ShutdownWithContext(ctx)
	}

	// Close the connection pool last, after every module has stopped and can no
	// longer issue queries. Without this the pool outlives the application and
	// its connections are only released when the process exits.
	var dbErr error
	if sqlDB, err := app.db.DB(); err == nil {
		dbErr = sqlDB.Close()
	} else {
		dbErr = err
	}

	return errors.Join(eventsErr, telemetryErr, stopErr, kernelErr, gatewayErr, dbErr, webhookSubErr)
}

func (app *App) kernelHandler() *fiber.App {
	server := fiber.New(app.fiberConfig())
	server.Use(recover.New())
	server.Use(requestid.New())
	server.Get("/healthz", func(ctx *fiber.Ctx) error {
		if !app.ready.Load() {
			return ctx.Status(fiber.StatusServiceUnavailable).JSON(map[string]string{"status": "starting"})
		}
		sqlDB, err := app.db.DB()
		if err != nil || sqlDB.PingContext(ctx.UserContext()) != nil {
			return ctx.Status(fiber.StatusServiceUnavailable).JSON(map[string]string{"status": "db_unavailable"})
		}
		return ctx.Status(fiber.StatusOK).JSON(map[string]any{
			"status":       "ok",
			"service":      "sannad-kernel",
			"capabilities": app.Registry.ListCapabilities(),
		})
	})
	server.Get("/livez", func(ctx *fiber.Ctx) error {
		return ctx.Status(fiber.StatusOK).JSON(map[string]string{"status": "ok"})
	})
	server.Get("/capabilities", func(ctx *fiber.Ctx) error {
		return ctx.Status(fiber.StatusOK).JSON(app.Registry.ListCapabilities())
	})
	// Expose Prometheus metrics at /metrics. The promhttp handler is a standard
	// net/http handler; adaptor.HTTPHandler bridges it into Fiber.
	server.Get("/metrics", adaptor.HTTPHandler(promhttp.Handler()))

	return server
}

func (app *App) gatewayHandler() *fiber.App {
	server := fiber.New(app.fiberConfig())
	server.Use(recover.New())
	server.Use(requestid.New())
	server.Use(cors.New(cors.Config{AllowOrigins: app.Config.CORSOrigins}))
	if app.Config.RateLimit > 0 {
		server.Use(limiter.New(limiter.Config{Max: app.Config.RateLimit}))
	}
	if app.Config.RateLimitPerTenant > 0 {
		app.tenantLimiter = limiter.New(limiter.Config{
			Max:        app.Config.RateLimitPerTenant,
			Expiration: time.Minute,
			KeyGenerator: func(ctx *fiber.Ctx) string {
				caller, ok := modulekit.CallerFrom(ctx.UserContext())
				if !ok {
					// requireAuth always sets the caller before this middleware
					// runs; this is only reachable if a future route calls it
					// out of order. Fail toward per-IP rather than a shared key
					// that would let every unauthenticated caller share one
					// counter.
					return "ip:" + ctx.IP()
				}
				return "tenant:" + caller.TenantID + ":" + caller.Subject
			},
			LimitReached: func(ctx *fiber.Ctx) error {
				return writeError(ctx, fiber.StatusTooManyRequests, errors.New("rate limit exceeded for this account"))
			},
		})
	}
	server.Get("/healthz", func(ctx *fiber.Ctx) error {
		if !app.ready.Load() {
			return ctx.Status(fiber.StatusServiceUnavailable).JSON(map[string]string{"status": "starting"})
		}
		sqlDB, err := app.db.DB()
		if err != nil || sqlDB.PingContext(ctx.UserContext()) != nil {
			return ctx.Status(fiber.StatusServiceUnavailable).JSON(map[string]string{"status": "db_unavailable"})
		}
		return ctx.Status(fiber.StatusOK).JSON(map[string]string{"status": "ok", "service": "sannad-gateway"})
	})
	server.Get("/livez", func(ctx *fiber.Ctx) error {
		return ctx.Status(fiber.StatusOK).JSON(map[string]string{"status": "ok"})
	})
	server.Get("/api/v1/openapi.json", app.handleOpenAPI)
	server.Post("/api/v1/auth/register", app.handleRegister)
	server.Post("/api/v1/auth/login", app.handleLogin)
	// Unauthenticated like login itself: the API key is the credential being
	// presented, not something a caller already holds a session to prove. A
	// service account's tenant comes from the key, never from this request —
	// see LoginWithAPIKey's own doc comment.
	server.Post("/api/v1/auth/service-login", app.handleServiceLogin)
	server.Get("/api/v1/me", app.requireAuth(), app.handleMe)
	server.Get("/api/v1/me/export", app.requireAuth(), app.handleExportMe)
	server.Delete("/api/v1/me", app.requireAuth(), app.handleDeleteMe)

	// Service account management. Behind requireAuth like webhook endpoint
	// management: an authenticated caller manages service accounts for their
	// own tenant, taken from the signed token — the same rule every other
	// authenticated route here follows.
	server.Post("/api/v1/service-accounts", app.requireAuth(), app.requirePermission("service_accounts:write"), app.handleCreateServiceAccount)
	server.Get("/api/v1/service-accounts", app.requireAuth(), app.requirePermission("service_accounts:read"), app.handleListServiceAccounts)
	server.Delete("/api/v1/service-accounts/:id", app.requireAuth(), app.requirePermission("service_accounts:write"), app.handleRevokeServiceAccount)

	// Role management: who holds what within the caller's own tenant. Gated
	// on "roles:write"/"roles:read" — only "admin" and "owner" hold either by
	// default (see auth.DefaultRoleCatalog), so an ordinary member cannot
	// grant itself anything.
	server.Post("/api/v1/identities/:id/roles", app.requireAuth(), app.requirePermission("roles:write"), app.handleAssignRole)
	server.Delete("/api/v1/identities/:id/roles/:role", app.requireAuth(), app.requirePermission("roles:write"), app.handleRevokeRole)
	server.Get("/api/v1/identities/:id/roles", app.requireAuth(), app.requirePermission("roles:read"), app.handleListIdentityRoles)

	// Audit trail. "audit:read" is intentionally not in any default role's
	// permission set except "owner" — compliance review is not something
	// "admin" gets automatically, unlike webhook or service-account
	// management, which any operator managing integrations needs day to day.
	server.Get("/api/v1/audit-events", app.requireAuth(), app.requirePermission("audit:read"), app.handleListAuditEvents)

	// Admin operations on a single account. "identities:*" is deliberately
	// not in any default role's permissions except "owner" — the same
	// reasoning as "audit:read": this is more sensitive than webhook or
	// service-account management, which "admin" gets by default.
	server.Post("/api/v1/identities/:id/lock", app.requireAuth(), app.requirePermission("identities:write"), app.handleLockAccount)
	server.Post("/api/v1/identities/:id/unlock", app.requireAuth(), app.requirePermission("identities:write"), app.handleUnlockAccount)
	server.Post("/api/v1/identities/:id/revoke-sessions", app.requireAuth(), app.requirePermission("identities:write"), app.handleRevokeAllSessions)
	server.Delete("/api/v1/identities/:id", app.requireAuth(), app.requirePermission("identities:write"), app.handleDeleteAccount)

	// The capability bridge. One endpoint serves every exposed capability, so a
	// module gets an external API by declaring Exposed rather than by anyone
	// hand-writing a proto and a service for it.
	//
	// Behind requireAuth like every other authenticated route: tenant and caller
	// come from the signed token, so the bridge introduces no new tenant path.
	// That is what keeps ADR 0005 intact — a bridge that accepted a tenant from
	// the request would be a cross-tenant read with extra steps.
	server.Post("/api/v1/capabilities/:ref", app.requireAuth(),
		adaptHTTPWithUserContext(bridge.Dispatch(app.Registry, app.dispatchCapability)))
	server.Get("/api/v1/capabilities", app.requireAuth(),
		adaptHTTPWithUserContext(bridge.ListHandler(app.Registry)))

	// Webhook endpoint management. Handlers read the tenant from
	// ctx.UserContext(), the same signed-token context every other
	// authenticated route uses — there is no tenant parameter for a caller to
	// forge.
	server.Post("/api/v1/webhooks/endpoints", app.requireAuth(), app.requirePermission("webhooks:write"), app.handleCreateWebhookEndpoint)
	server.Get("/api/v1/webhooks/endpoints", app.requireAuth(), app.requirePermission("webhooks:read"), app.handleListWebhookEndpoints)
	server.Get("/api/v1/webhooks/endpoints/:id", app.requireAuth(), app.requirePermission("webhooks:read"), app.handleGetWebhookEndpoint)
	server.Patch("/api/v1/webhooks/endpoints/:id", app.requireAuth(), app.requirePermission("webhooks:write"), app.handleUpdateWebhookEndpoint)
	server.Delete("/api/v1/webhooks/endpoints/:id", app.requireAuth(), app.requirePermission("webhooks:write"), app.handleDeleteWebhookEndpoint)
	server.Post("/api/v1/webhooks/endpoints/:id/rotate-secret", app.requireAuth(), app.requirePermission("webhooks:write"), app.handleRotateWebhookSecret)
	server.Post("/api/v1/webhooks/endpoints/:id/replay", app.requireAuth(), app.requirePermission("webhooks:write"), app.handleReplayWebhookEndpoint)
	server.Get("/api/v1/webhooks/endpoints/:id/deliveries", app.requireAuth(), app.requirePermission("webhooks:read"), app.handleListWebhookDeliveries)
	server.Post("/api/v1/webhooks/deliveries/:id/replay", app.requireAuth(), app.requirePermission("webhooks:write"), app.handleReplayWebhookDelivery)
	for _, mount := range app.httpRoutes {
		mount(server.Group("/api/v1"), app.requireAuth())
	}
	if app.Auth.HasOIDC() {
		server.Get("/api/v1/oauth2/:provider/authorize", app.handleOAuthAuthorize)
		server.Get("/api/v1/oauth2/:provider/callback", app.handleOAuthCallback)
	}
	if app.Auth.HasSAML() {
		server.Get("/api/v1/auth/saml/metadata", app.handleSAMLMetadata)
		server.Get("/api/v1/auth/saml/:idp/login", app.handleSAMLLogin)
		server.Post("/api/v1/auth/saml/acs", app.handleSAMLACS)
	}

	return server
}

// requireAuth is a Fiber middleware that validates the bearer token and injects
// CallerInfo into the request context. Protected handlers read the caller with
// modulekit.CallerFrom(ctx.UserContext()) instead of re-validating the token.
func (app *App) requireAuth() fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		token := bearerToken(ctx.Get("Authorization"))
		if token == "" {
			return writeError(ctx, fiber.StatusUnauthorized, errors.New("missing bearer token"))
		}
		// The tenant comes from the signed token and from nowhere else. The
		// X-Tenant-ID header and ?tenant= parameter that used to feed it were
		// client-controlled, which meant any authenticated caller could read
		// another tenant's rows. See ADR 0005.
		subject, tenantID, err := app.Auth.ValidateForCaller(token)
		if err != nil {
			// Intentionally vague — do not reveal session state to callers.
			return writeError(ctx, fiber.StatusUnauthorized, errors.New("invalid or expired token"))
		}

		reqCtx := modulekit.WithCaller(ctx.UserContext(), modulekit.CallerInfo{
			Subject:  subject,
			TenantID: tenantID,
		})
		// Storage isolation reads the tenant from the Kayan tenant context, so
		// the same value must be placed there for the GORM callbacks to see it.
		reqCtx = tenant.WithTenantID(reqCtx, tenantID)
		ctx.SetUserContext(reqCtx)
		if app.tenantLimiter != nil {
			return app.tenantLimiter(ctx)
		}
		return ctx.Next()
	}
}

// requirePermission gates a route on the caller holding permission within
// its own tenant. Must run after requireAuth: it reads the caller and
// tenant requireAuth already placed in context, and checks nothing itself
// if they are absent.
//
// A denial is 403, not 404: unlike a cross-tenant resource lookup (see
// GetEndpoint, RevokeServiceAccount), there is no tenant boundary to hide
// here — the caller legitimately belongs to this tenant and is being told
// plainly that its role does not cover this action.
func (app *App) requirePermission(permission string) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		caller, ok := modulekit.CallerFrom(ctx.UserContext())
		if !ok {
			return writeError(ctx, fiber.StatusUnauthorized, errors.New("missing bearer token"))
		}
		if err := app.Auth.RequirePermission(ctx.UserContext(), caller.Subject, permission); err != nil {
			return writeError(ctx, fiber.StatusForbidden, err)
		}
		return ctx.Next()
	}
}

// adaptHTTPWithUserContext bridges a net/http handler into Fiber without
// dropping the authenticated context installed by requireAuth.
//
// Fiber's stock adaptor reconstructs an http.Request from fasthttp and does not
// attach Ctx.UserContext. That silently removed caller and tenant values before
// capability dispatch. Keeping this adapter at the transport seam lets bridge
// remain ordinary net/http while preserving the signed-session context.
func adaptHTTPWithUserContext(handler http.Handler) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		request, err := adaptor.ConvertRequest(ctx, true)
		if err != nil {
			return err
		}
		request = request.WithContext(ctx.UserContext())

		response := &fiberResponseWriter{header: make(http.Header), status: http.StatusOK}
		handler.ServeHTTP(response, request)

		for name, values := range response.header {
			for _, value := range values {
				ctx.Response().Header.Add(name, value)
			}
		}
		ctx.Status(response.status)
		return ctx.Send(response.body.Bytes())
	}
}

type fiberResponseWriter struct {
	header      http.Header
	status      int
	wroteHeader bool
	body        bytes.Buffer
}

func (w *fiberResponseWriter) Header() http.Header { return w.header }

func (w *fiberResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
}

func (w *fiberResponseWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.body.Write(body)
}

// dispatchCapability is the bridge's call into the kernel.
//
// The context comes from the request, which the auth middleware has already
// populated with the caller and tenant from the signed token. Passing it through
// unchanged is the whole point: a capability handler cannot tell whether it was
// reached from inside the process or from the network, which is the ADR 0001
// claim it exists to make true.
func (app *App) dispatchCapability(r *http.Request, ref modulekit.CapabilityRef, payload modulekit.RawPayload) (any, error) {
	return app.dispatchCapabilityCtx(r.Context(), ref, payload)
}

// dispatchCapabilityCtx is the single dispatch both transports share.
//
// One function on purpose: a capability reachable over HTTP but not gRPC, or
// subject to a different check on one of them, would be a security boundary
// nobody could reason about.
func (app *App) dispatchCapabilityCtx(ctx context.Context, ref modulekit.CapabilityRef, payload modulekit.RawPayload) (any, error) {
	return app.Bus.Call(ctx, ref, payload)
}

// resolveTenant determines the tenant for an unauthenticated request.
//
// The subdomain is authoritative when the deployment is configured with a base
// domain: on tenant-a.sannad.example the tenant cannot be chosen by the caller.
// The request body is consulted only when no subdomain tenant is present, which
// covers single-domain and local deployments.
//
// Authenticated requests never reach this path — their tenant comes from the
// signed session token.
func (app *App) resolveTenant(ctx *fiber.Ctx, fallback string) string {
	if app.tenantResolver != nil {
		req, err := adaptor.ConvertRequest(ctx, false)
		if err == nil {
			info := tenant.ResolveInfoFromRequest(req)
			if resolved, rerr := app.tenantResolver.Resolve(ctx.UserContext(), info); rerr == nil && resolved != "" {
				return resolved
			}
		}
	}
	return fallback
}

// checkTenant refuses a tenant that is not provisioned and active.
//
// The subdomain path is already validated by the resolver wrapper, but the body
// fallback is not: it is read straight from the request. Without this check a
// caller on a single-domain deployment could name any tenant and have it work,
// which is the whole gap the directory closes.
//
// A directory error fails the request. Treating an unreachable database as an
// acceptable tenant would readmit every suspended tenant during an outage.
func (app *App) checkTenant(ctx *fiber.Ctx, tenantID string) error {
	if app.tenants == nil {
		return nil
	}
	return app.tenants.Check(ctx.UserContext(), tenantID)
}

// tenantRejection maps a directory refusal to a status code.
//
// Unknown and suspended are deliberately answered the same way to a caller: the
// difference is operationally important and recorded in the log, but telling an
// unauthenticated caller which tenants exist is an enumeration oracle.
func tenantRejection(ctx *fiber.Ctx, err error) error {
	slog.WarnContext(ctx.UserContext(), "tenant rejected at edge",
		"request_id", ctx.GetRespHeader("X-Request-Id"),
		"err", err.Error())
	return writeError(ctx, fiber.StatusNotFound, errors.New("unknown tenant"))
}

func (app *App) handleRegister(ctx *fiber.Ctx) error {
	payload, err := decodeCredentials(ctx)
	if err != nil {
		return writeError(ctx, fiber.StatusBadRequest, err)
	}

	tenantID := app.resolveTenant(ctx, payload.Tenant)
	if tenantID == "" {
		return writeError(ctx, fiber.StatusBadRequest, errors.New("tenant is required"))
	}
	if err := app.checkTenant(ctx, tenantID); err != nil {
		// Self-service signup needs registration to be able to create a tenant.
		// A deployment that provisions out of band does not, and for it an
		// unknown tenant must stay an error: silently creating one is how data
		// ends up under an identifier nobody approved.
		//
		// Only an unknown tenant may be provisioned this way. A suspended one is
		// an operator decision and is never overridden by a registration.
		if !app.Config.TenantSelfProvision || !errors.Is(err, tenancy.ErrUnknownTenant) {
			return tenantRejection(ctx, err)
		}
		if perr := app.tenants.Provision(ctx.UserContext(), tenantID, tenantID); perr != nil {
			return writeError(ctx, fiber.StatusInternalServerError, perr)
		}
	}

	if err := app.Auth.Register(ctx.UserContext(), tenantID, payload.Email, payload.Password); err != nil {
		// Log actual reason; return a safe message that doesn't reveal account existence.
		slog.WarnContext(ctx.UserContext(), "registration rejected",
			"request_id", ctx.GetRespHeader("X-Request-Id"),
			"err", err.Error(),
		)
		return ctx.Status(fiber.StatusConflict).JSON(map[string]string{"error": "email already in use"})
	}

	return ctx.Status(fiber.StatusCreated).JSON(map[string]string{"status": "registered"})
}

func (app *App) handleLogin(ctx *fiber.Ctx) error {
	payload, err := decodeCredentials(ctx)
	if err != nil {
		return writeError(ctx, fiber.StatusBadRequest, err)
	}

	tenantID := app.resolveTenant(ctx, payload.Tenant)
	if tenantID == "" {
		return writeError(ctx, fiber.StatusBadRequest, errors.New("tenant is required"))
	}

	// A suspended tenant's users must not be able to log in, which is what makes
	// a suspension a suspension rather than a note in a table.
	if err := app.checkTenant(ctx, tenantID); err != nil {
		return tenantRejection(ctx, err)
	}

	result, err := app.Auth.Login(ctx.UserContext(), tenantID, payload.Email, payload.Password)
	if err != nil {
		// Intentionally vague — do not reveal whether the account exists (OWASP A07).
		return ctx.Status(fiber.StatusUnauthorized).JSON(map[string]string{"error": "invalid credentials"})
	}

	return ctx.Status(fiber.StatusOK).JSON(result)
}

func (app *App) handleMe(ctx *fiber.Ctx) error {
	// CallerInfo was injected by the requireAuth middleware.
	caller, _ := modulekit.CallerFrom(ctx.UserContext())
	return ctx.Status(fiber.StatusOK).JSON(map[string]string{"subject": caller.Subject})
}

// handleExportMe returns everything this deployment holds on the caller's
// own account — a data subject access request, self-served rather than
// requiring an operator, exactly like /api/v1/me itself.
func (app *App) handleExportMe(ctx *fiber.Ctx) error {
	caller, _ := modulekit.CallerFrom(ctx.UserContext())
	export, err := app.Auth.ExportAccountData(ctx.UserContext(), caller.Subject)
	if err != nil {
		return writeAdminError(ctx, err)
	}
	return ctx.Status(fiber.StatusOK).JSON(export)
}

// handleDeleteMe erases the caller's own personal data. Self-service,
// unlike lock/unlock/revoke-sessions, which are operator actions on someone
// else's account — deleting one's own data needs no separate permission
// grant beyond having an authenticated session in the first place.
func (app *App) handleDeleteMe(ctx *fiber.Ctx) error {
	caller, _ := modulekit.CallerFrom(ctx.UserContext())
	if err := app.Auth.DeleteAccount(ctx.UserContext(), caller.Subject); err != nil {
		return writeAdminError(ctx, err)
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}

func decodeCredentials(ctx *fiber.Ctx) (credentialsPayload, error) {
	var payload credentialsPayload
	if err := ctx.BodyParser(&payload); err != nil {
		return credentialsPayload{}, err
	}

	if payload.Email == "" || payload.Password == "" {
		return credentialsPayload{}, errors.New("email and password are required")
	}

	return payload, nil
}

func writeError(ctx *fiber.Ctx, statusCode int, err error) error {
	if statusCode >= 500 {
		// Never expose internal error details to API clients (OWASP A02).
		// Log the real cause server-side so operators can diagnose it.
		slog.ErrorContext(ctx.UserContext(), "internal error",
			"path", ctx.Path(),
			"method", ctx.Method(),
			"request_id", ctx.GetRespHeader("X-Request-Id"),
			"err", err.Error(),
		)
		return ctx.Status(statusCode).JSON(map[string]string{"error": "internal server error"})
	}
	return ctx.Status(statusCode).JSON(map[string]string{"error": err.Error()})
}

// jsonErrorHandler converts any unhandled Fiber error (including 404/405) to a
// JSON response. Without this, Fiber returns HTML error pages by default.
func jsonErrorHandler(ctx *fiber.Ctx, err error) error {
	code := fiber.StatusInternalServerError
	msg := "internal server error"
	var fe *fiber.Error
	if errors.As(err, &fe) {
		code = fe.Code
		msg = fe.Message
	} else {
		slog.ErrorContext(ctx.UserContext(), "unhandled error",
			"path", ctx.Path(),
			"method", ctx.Method(),
			"request_id", ctx.GetRespHeader("X-Request-Id"),
			"err", err.Error(),
		)
	}
	return ctx.Status(code).JSON(map[string]string{"error": msg})
}

func bearerToken(authorizationHeader string) string {
	parts := strings.SplitN(authorizationHeader, " ", 2)
	if len(parts) == 2 && parts[0] == "Bearer" {
		return parts[1]
	}

	return ""
}

func openDatabase(dsn string, cfg config.Config) (*gorm.DB, error) {
	var db *gorm.DB
	var err error
	if strings.HasPrefix(dsn, "postgres://") ||
		strings.HasPrefix(dsn, "postgresql://") ||
		strings.HasPrefix(dsn, "host=") {
		db, err = gorm.Open(postgres.Open(dsn), storage.GormConfig())
	} else {
		db, err = gorm.Open(sqlite.Open(dsn), storage.GormConfig())
	}
	if err != nil {
		return nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	if cfg.DBMaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(cfg.DBMaxOpenConns)
	}
	if cfg.DBMaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(cfg.DBMaxIdleConns)
	}
	if cfg.DBConnMaxLifetimeSecs > 0 {
		sqlDB.SetConnMaxLifetime(time.Duration(cfg.DBConnMaxLifetimeSecs) * time.Second)
	}
	return db, nil
}

// buildGRPCServer constructs the kernel gRPC server with auth and
// entrypoint-registered services. Called from Start, after registration. The
// server is created even when GRPCAddr is empty so that a started app can
// always be shut down.
func (app *App) buildGRPCServer() *grpc.Server {
	srv := grpc.NewServer(grpc.UnaryInterceptor(app.grpcAuthInterceptor))

	// The kernel registers this one itself rather than leaving it to an
	// entrypoint. It is the generic dispatch that gives every exposed capability
	// a gRPC surface, so a deployment that forgot to wire it would silently have
	// no external gRPC API at all — and the modules affected would be the ones
	// whose authors never wrote gRPC code, which is the case it exists for.
	kernelV1.RegisterCapabilitiesServer(srv, bridge.NewGRPCService(
		app.Registry, app.dispatchCapabilityCtx, json.Marshal))

	for _, register := range app.grpcServices {
		register(srv)
	}
	return srv
}

// grpcAuthInterceptor validates the Bearer token from gRPC metadata and
// injects CallerInfo into the request context, mirroring the gateway
// requireAuth middleware.
func (app *App) grpcAuthInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, grpcstatus.Error(codes.Unauthenticated, "missing metadata")
	}
	values := md.Get("authorization")
	if len(values) == 0 {
		return nil, grpcstatus.Error(codes.Unauthenticated, "missing bearer token")
	}
	token := bearerToken(values[0])
	if token == "" {
		return nil, grpcstatus.Error(codes.Unauthenticated, "missing bearer token")
	}
	// As on the HTTP gateway, the tenant comes from the signed token. The
	// x-tenant-id metadata key that used to supply it was caller-controlled.
	subject, tenantID, err := app.Auth.ValidateForCaller(token)
	if err != nil {
		return nil, grpcstatus.Error(codes.Unauthenticated, "invalid or expired token")
	}
	ctx = modulekit.WithCaller(ctx, modulekit.CallerInfo{
		Subject:  subject,
		TenantID: tenantID,
	})
	ctx = tenant.WithTenantID(ctx, tenantID)
	return handler(ctx, req)
}

// handleOAuthAuthorize redirects the user to the OIDC provider's authorization
// endpoint. State, PKCE, and a nonce are generated and stored server-side by
// Kayan's OIDC strategy (see auth.Service.GetOAuthURL) — there is no cookie
// for this handler to set. The previous flow's cookie-based CSRF token is
// gone along with flow.OIDCManager, which it existed to compensate for: that
// manager's callback took no state parameter at all.
func (app *App) handleOAuthAuthorize(ctx *fiber.Ctx) error {
	provider := ctx.Params("provider")
	authURL, err := app.Auth.GetOAuthURL(ctx.UserContext(), provider)
	if err != nil {
		return writeError(ctx, fiber.StatusBadRequest, err)
	}
	return ctx.Redirect(authURL, fiber.StatusFound)
}

// handleOAuthCallback exchanges the authorization code for an identity via
// the OIDC provider and creates a session. State validation — including
// single-use enforcement — happens inside Kayan's strategy against the
// server-side record GetOAuthURL created; an invalid, expired, or replayed
// state is reported back as an ordinary callback failure below.
func (app *App) handleOAuthCallback(ctx *fiber.Ctx) error {
	provider := ctx.Params("provider")
	state := ctx.Query("state")
	if state == "" {
		return writeError(ctx, fiber.StatusBadRequest, errors.New("missing OAuth2 state"))
	}

	code := ctx.Query("code")
	if code == "" {
		return writeError(ctx, fiber.StatusBadRequest, errors.New("missing authorization code"))
	}

	// The tenant is taken from the host the callback landed on, matching the
	// host the flow was started from.
	tenantID := app.resolveTenant(ctx, "")
	if tenantID == "" {
		return writeError(ctx, fiber.StatusBadRequest, errors.New("tenant is required"))
	}

	// Checked here too: an OAuth callback is a second way into the same session,
	// and a suspension that only blocked password login would be no suspension.
	if err := app.checkTenant(ctx, tenantID); err != nil {
		return tenantRejection(ctx, err)
	}

	result, err := app.Auth.HandleOAuthCallback(ctx.UserContext(), tenantID, provider, state, code)
	if err != nil {
		slog.WarnContext(ctx.UserContext(), "oauth2 callback failed",
			"provider", provider,
			"request_id", ctx.GetRespHeader("X-Request-Id"),
			"err", err.Error(),
		)
		return writeError(ctx, fiber.StatusUnauthorized, errors.New("OAuth2 authentication failed"))
	}
	return ctx.Status(fiber.StatusOK).JSON(result)
}

// parseOIDCProviders decodes a JSON map of OIDC provider configurations.
// Expected format: {"google":{"issuer":"...","client_id":"...","client_secret":"...","redirect_url":"..."}}
func parseOIDCProviders(raw string) (map[string]auth.OIDCProviderConfig, error) {
	var providers map[string]auth.OIDCProviderConfig
	if err := json.Unmarshal([]byte(raw), &providers); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	return providers, nil
}

// parseSAMLProviders decodes a JSON map of SAML identity provider
// configurations. Expected format:
// {"okta":{"tenant_id":"acme","metadata_url":"https://okta.example.com/metadata"}}
func parseSAMLProviders(raw string) (map[string]auth.SAMLProviderConfig, error) {
	var providers map[string]auth.SAMLProviderConfig
	if err := json.Unmarshal([]byte(raw), &providers); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	return providers, nil
}

// handleSAMLMetadata serves this deployment's SP metadata document,
// unauthenticated like the OIDC discovery endpoints it sits beside — an
// identity provider administrator fetches it while configuring the
// integration, before any session exists to authenticate with.
func (app *App) handleSAMLMetadata(ctx *fiber.Ctx) error {
	doc, err := app.Auth.SAMLMetadata()
	if err != nil {
		return writeError(ctx, fiber.StatusInternalServerError, err)
	}
	ctx.Set("Content-Type", "application/samlmetadata+xml")
	return ctx.Send(doc)
}

// handleSAMLLogin redirects the caller to idp's SSO endpoint. Unlike
// handleOAuthAuthorize, there is no tenant to resolve here at all: the
// tenant is fixed by which identity provider this path names, decided when
// the provider was configured.
func (app *App) handleSAMLLogin(ctx *fiber.Ctx) error {
	idpID := ctx.Params("idp")
	returnURL := ctx.Query("return_url")
	redirectURL, err := app.Auth.InitiateSAMLLogin(ctx.UserContext(), idpID, returnURL)
	if err != nil {
		return writeError(ctx, fiber.StatusBadRequest, err)
	}
	return ctx.Redirect(redirectURL, fiber.StatusFound)
}

// handleSAMLACS is the Assertion Consumer Service endpoint every configured
// identity provider posts its response to. Unauthenticated: this is how a
// session gets created in the first place, and which pending login the
// response answers is resolved from RelayState by Kayan's own session
// store, not by the caller.
func (app *App) handleSAMLACS(ctx *fiber.Ctx) error {
	samlResponse := ctx.FormValue("SAMLResponse")
	if samlResponse == "" {
		return writeError(ctx, fiber.StatusBadRequest, errors.New("missing SAMLResponse"))
	}
	relayState := ctx.FormValue("RelayState")

	result, err := app.Auth.HandleSAMLResponse(ctx.UserContext(), samlResponse, relayState)
	if err != nil {
		slog.WarnContext(ctx.UserContext(), "saml acs failed",
			"request_id", ctx.GetRespHeader("X-Request-Id"),
			"err", err.Error(),
		)
		return writeError(ctx, fiber.StatusUnauthorized, errors.New("SAML authentication failed"))
	}
	return ctx.Status(fiber.StatusOK).JSON(result)
}

// Tenants returns the tenant directory.
//
// Exposed so an entrypoint can provision tenants at deploy time and an operator
// tool can suspend one. A deployment that provisions out of band has no other
// way to create the tenants its users log into.
func (app *App) Tenants() *tenancy.Directory {
	return app.tenants
}
