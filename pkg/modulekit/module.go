package modulekit

import "context"

type Handler func(ctx context.Context, request any) (any, error)

type CapabilityRef struct {
	Name    string
	Version string
}

func (ref CapabilityRef) Key() string {
	return ref.Name + "@" + ref.Version
}

type Descriptor struct {
	ID       string
	Kind     string
	Provides []CapabilityRef
	Consumes []CapabilityRef

	// Offers are the hook points this module opens to participation by modules
	// it does not know about. Declaring one is a public API commitment: removing
	// or changing it is a breaking change. See ADR 0003.
	Offers []HookPoint

	// Storage declares where this module keeps its data.
	//
	// Required. An empty value is refused at registration rather than defaulted,
	// because a default would make the declaration meaningless for exactly the
	// modules whose authors never considered the question — which is the
	// population this field exists for.
	Storage StorageMode

	// Exposed are the capabilities this module publishes to callers outside the
	// process — the external HTTP and gRPC bridge.
	//
	// Registration is not publication. A module registers a capability so other
	// modules can call it; that must not also put it on the public internet,
	// because the two decisions have different blast radii and different
	// reviewers. Empty means externally invisible, which is the safe default for
	// a module whose author never considered the question.
	//
	// Every entry must also appear in Provides: a module cannot expose a
	// capability it does not serve.
	Exposed []CapabilityRef
}

// StorageMode declares where a module keeps its data.
//
// A module may use the Store the kernel hands it, or manage its own persistence
// — its own database, a remote API, memory, nothing at all. Both are supported
// and neither is forced. What is not supported is leaving it unsaid: "I brought
// my own storage" and "I forgot to take one" must not look identical to an
// operator reading a startup log.
//
// The difference is not stylistic. On the kernel store, tenant isolation is
// applied underneath every query and a module cannot forget it: a scoped
// operation with no tenant in context fails rather than running unscoped. Off
// it, isolation is the module author's responsibility, and nothing outside the
// module can verify they got it right — the startup audit inspects database
// tables, so data kept anywhere else is invisible to it.
type StorageMode string

const (
	// StorageKernel means the module uses the Store passed to it. Tenant
	// isolation is enforced by the adapter, and the SDK helpers that need a
	// transaction — Idempotent, RunSaga, Project — are available.
	StorageKernel StorageMode = "kernel"

	// StorageOwn means the module manages its own persistence and is responsible
	// for tenant isolation itself.
	//
	// A legitimate choice: a module wrapping an existing datastore, or one whose
	// data lives in a system the kernel has no business knowing about. The cost
	// is that the isolation guarantee becomes a claim rather than a mechanism,
	// and the SDK helpers are unavailable — they must write their marker in the
	// same transaction as the module's own work, which requires a Store.
	StorageOwn StorageMode = "own"

	// StorageNone means the module persists nothing: a pure computation, a
	// transformer, a module that only calls others.
	StorageNone StorageMode = "none"
)

// Valid reports whether a storage mode is one of the declared values.
func (m StorageMode) Valid() bool {
	switch m {
	case StorageKernel, StorageOwn, StorageNone:
		return true
	}
	return false
}

type Registrar interface {
	RegisterCapability(ref CapabilityRef, handler Handler) error
}

// HookRegistrarFrom returns the hook surface of a Registrar, if it has one.
//
// Registrar is kept narrow so existing modules and test doubles continue to
// compile. A module that offers or subscribes to hooks asks for the surface and
// handles its absence, rather than every implementation growing two methods it
// does not use.
func HookRegistrarFrom(reg Registrar) (HookRegistrar, bool) {
	hr, ok := reg.(HookRegistrar)
	return hr, ok
}

// Module is the core interface every embedded module must implement.
//
// Lifecycle:
//
//	Install(reg) → called once when the module is registered in the kernel.
//	               Register capabilities and run schema migrations here.
//	Start(ctx)   → called after all modules are installed. Start background
//	               workers, open connections, seed dev data here.
//	Stop(ctx)    → called on graceful shutdown in reverse registration order.
//	               Release resources, flush buffers here.
type Module interface {
	Descriptor() Descriptor
	Install(reg Registrar) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}
