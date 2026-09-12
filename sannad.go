// Package sannad is the public application API for the Sannad headless
// Business Operating System framework.
package sannad

import (
	"context"
	"time"

	"github.com/getkayan/kayan/core/tenant"
	"github.com/gofiber/fiber/v2"
	"github.com/sannados/sannad/internal/app/bootstrap"
	"github.com/sannados/sannad/internal/kernel/wasmhost"
	publicconfig "github.com/sannados/sannad/pkg/config"
	"github.com/sannados/sannad/pkg/modulekit"
	"google.golang.org/grpc"
)

// Config is Sannad's public runtime configuration.
type Config = publicconfig.Config

// ConfigFromEnv reads the documented SANNAD_* environment variables.
func ConfigFromEnv() Config { return publicconfig.FromEnv() }

// WASMManifest is the operator-approved identity and publication surface of a
// sandboxed module. It is not trusted metadata read from guest bytes: a guest
// cannot grant itself a capability, an outbound call, or a hook subscription
// by embedding a more permissive declaration in its own bytes.
type WASMManifest struct {
	ID         string
	ABIVersion uint32
	Provides   []modulekit.CapabilityRef
	Exposed    []modulekit.CapabilityRef

	// Consumes is the exact set of capabilities this guest may call through
	// the ABI v2 host-call surface (ADR 0007). Refused for a guest declaring
	// ABIVersion 1 — a v1 guest has no import table to exercise it.
	Consumes []modulekit.CapabilityRef

	// PublishesEvents is the exact set of logical event names this guest may
	// publish. ABI v2 only. Wildcards are forbidden; a name here is matched
	// exactly.
	PublishesEvents []string

	// SubscribesHooks is the set of hook points this guest participates in.
	// ABI v2 only.
	SubscribesHooks []WASMHookGrant
}

// WASMHookGrant is one approved hook subscription: which point, what kind of
// participation the operator approved it for, and its dispatch priority.
type WASMHookGrant struct {
	Ref      modulekit.HookRef
	Kind     modulekit.HookKind
	Priority int
}

// CurrentWASMABIVersion is the guest ABI understood by this release. Version
// 2 adds the host-call surface (outbound capability calls, event publishes,
// and hook subscriptions) on top of the same guest exports and envelope
// version 1 already used; a manifest may still declare ABIVersion 1 for a
// guest with no need of it.
const CurrentWASMABIVersion uint32 = wasmhost.CurrentABIVersion

// WASMConfig contains approved metadata, guest bytes, and operator-owned limits.
type WASMConfig struct {
	Manifest         WASMManifest
	Wasm             []byte
	MaxMemoryPages   uint32
	Timeout          time.Duration
	MaxResponseBytes uint32
	Stdout           func([]byte)
	Stderr           func([]byte)

	// Resource limits for the ABI v2 host-call surface. Zero uses the
	// default for each; only meaningful when Manifest grants Consumes,
	// PublishesEvents, or SubscribesHooks.
	MaxOutboundRequestBytes uint32
	MaxResultBytes          uint32
	MaxResultHandles        uint32
	MaxEventPayloadBytes    uint32
	MaxCallDepth            uint32
}

// App is a headless Sannad application. It wraps the one production runtime;
// public consumers never need to import an internal package.
type App struct {
	inner *bootstrap.App
}

// New validates config and initializes storage, Kayan, tenancy, events, and
// the kernel runtime. It does not start listeners; call Start first, then the
// desired Run* methods.
func New(config Config) (*App, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	inner, err := bootstrap.New(config)
	if err != nil {
		return nil, err
	}
	return &App{inner: inner}, nil
}

// Store returns the engine-neutral store modules receive.
func (a *App) Store() modulekit.Store { return a.inner.Store() }

// Events returns the application event publisher. Modules publish through it
// but lifecycle ownership remains with the application.
func (a *App) Events() modulekit.EventPublisher { return a.inner.EventPublisher() }

// RegisterModule installs an embedded Go module into the real kernel runtime.
func (a *App) RegisterModule(module modulekit.Module) error {
	return a.inner.RegisterModule(module)
}

// RegisterWASM loads and installs an operator-approved sandboxed module. The
// platform's own capability bus and event publisher are wired in for
// Consumes/PublishesEvents automatically — there is no field for a caller to
// supply its own, since a guest's outbound calls must resolve through the
// same registry every module uses.
func (a *App) RegisterWASM(ctx context.Context, config WASMConfig) error {
	hooks := make([]wasmhost.HookGrant, len(config.Manifest.SubscribesHooks))
	for i, grant := range config.Manifest.SubscribesHooks {
		hooks[i] = wasmhost.HookGrant{Ref: grant.Ref, Kind: grant.Kind, Priority: grant.Priority}
	}

	return a.inner.RegisterWASM(ctx, wasmhost.ModuleConfig{
		Manifest: wasmhost.ApprovedManifest{
			ID:              config.Manifest.ID,
			ABIVersion:      config.Manifest.ABIVersion,
			Provides:        append([]modulekit.CapabilityRef(nil), config.Manifest.Provides...),
			Exposed:         append([]modulekit.CapabilityRef(nil), config.Manifest.Exposed...),
			Consumes:        append([]modulekit.CapabilityRef(nil), config.Manifest.Consumes...),
			PublishesEvents: append([]string(nil), config.Manifest.PublishesEvents...),
			SubscribesHooks: hooks,
		},
		Wasm:                    append([]byte(nil), config.Wasm...),
		MaxMemoryPages:          config.MaxMemoryPages,
		Timeout:                 config.Timeout,
		MaxResponseBytes:        config.MaxResponseBytes,
		Stdout:                  config.Stdout,
		Stderr:                  config.Stderr,
		MaxOutboundRequestBytes: config.MaxOutboundRequestBytes,
		MaxResultBytes:          config.MaxResultBytes,
		MaxResultHandles:        config.MaxResultHandles,
		MaxEventPayloadBytes:    config.MaxEventPayloadBytes,
		MaxCallDepth:            config.MaxCallDepth,
	})
}

// RegisterScopedModels is retained for modules predating ScopedModelProvider.
func (a *App) RegisterScopedModels(models ...modulekit.TenantAware) error {
	kayanModels := make([]tenant.TenantAware, len(models))
	for i, model := range models {
		kayanModels[i] = model
	}
	return a.inner.RegisterScopedModels(kayanModels...)
}

// Call implements modulekit.Caller for direct, typed module-to-module calls.
func (a *App) Call(ctx context.Context, ref modulekit.CapabilityRef, request any) (any, error) {
	return a.inner.Bus.Call(ctx, ref, request)
}

// RegisterAuthenticatedRoutes mounts an optional custom Fiber projection.
// Transport-neutral capabilities require no custom route.
func (a *App) RegisterAuthenticatedRoutes(mount func(fiber.Router, fiber.Handler)) {
	a.inner.RegisterAuthenticatedRoutes(mount)
}

// RegisterGRPCService mounts an optional custom gRPC projection.
// Every exposed capability already has the generic capability service.
func (a *App) RegisterGRPCService(register func(*grpc.Server)) {
	a.inner.RegisterGRPCService(register)
}

func (a *App) Start(ctx context.Context) error      { return a.inner.Start(ctx) }
func (a *App) RunKernel(ctx context.Context) error  { return a.inner.RunKernel(ctx) }
func (a *App) RunGateway(ctx context.Context) error { return a.inner.RunGateway(ctx) }
func (a *App) RunGRPC(ctx context.Context) error    { return a.inner.RunGRPC(ctx) }
func (a *App) Shutdown(ctx context.Context) error   { return a.inner.Shutdown(ctx) }

var _ modulekit.Caller = (*App)(nil)
