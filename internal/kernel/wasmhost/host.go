package wasmhost

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/getkayan/kayan/core/tenant"
	"github.com/sannados/sannad/pkg/modulekit"
	"github.com/tetratelabs/wazero"
)

// Config bounds what a guest may do.
//
// Every field is a limit rather than a grant, because the default for untrusted
// code has to be "no". A guest gets memory, time, and whatever host functions
// are wired — nothing else. There is no filesystem, no network, no clock beyond
// what the host provides, and no way for the guest to ask for more.
type Config struct {
	// ModuleID identifies the guest in errors and logs.
	ModuleID string

	// Wasm is the compiled module.
	Wasm []byte

	// MaxMemoryPages caps linear memory, in 64 KiB pages. Zero uses the default.
	//
	// This is the sandbox's main resource bound: a guest that allocates in a loop
	// hits this and traps rather than exhausting the host.
	MaxMemoryPages uint32

	// Timeout bounds one invocation. Zero uses the default.
	//
	// Enforced by cancelling the guest's context, which wazero honours by
	// interrupting execution — this is the guarantee the embedded tier cannot
	// offer, where a subscriber ignoring its context leaves a goroutine nothing
	// can stop.
	Timeout time.Duration

	// Stdout and Stderr receive whatever the guest writes to those descriptors.
	//
	// The one real capability the sandbox grants, and it earns its place: without
	// it a guest panic is silent, and a plugin author debugging a sandbox with no
	// output has nothing to work with. nil discards.
	Stdout func([]byte)
	Stderr func([]byte)

	// MaxResponseBytes caps what a guest may return. Zero uses the default.
	//
	// The sandbox bounds the guest's memory, not the host's: without this a guest
	// returns a length that makes the host allocate arbitrarily.
	MaxResponseBytes uint32

	// V2 grants and bounds the ABI v2 host-call surface: outbound capability
	// calls, event publishes, and the recursion limit enforced at every WASM
	// capability's entry point. Nil means this guest has no v2 grants — its
	// sannad_v2 imports, if it has any, are refused rather than silently no-ops.
	V2 *V2Config
}

// V2Config is operator policy for ADR 0007's host-call ABI, never guest input.
// A guest cannot enlarge this from inside its own bytes.
type V2Config struct {
	// Caller resolves an outbound capability_call through the same kernel
	// registry any module uses. Required when Consumes is non-empty.
	Caller modulekit.Caller

	// Events publishes an outbound event_publish. Required when
	// PublishesEvents is non-empty.
	Events modulekit.EventPublisher

	// Consumes is the exact set of capabilities this guest may call.
	Consumes []modulekit.CapabilityRef

	// PublishesEvents is the exact set of logical event names this guest may
	// publish. The host derives the tenant-scoped broker subject; a guest
	// never supplies one.
	PublishesEvents []string

	// Resource limits. Zero uses the default.
	MaxOutboundRequestBytes uint32
	MaxResultBytes          uint32
	MaxResultHandles        uint32
	MaxEventPayloadBytes    uint32
	MaxCallDepth            uint32
}

// Defaults for the v2 host-call surface, chosen for the same reason as the v1
// defaults: usable for real work, far below anything that threatens the host.
const (
	DefaultMaxOutboundRequestBytes = 1 << 20 // 1 MiB
	DefaultMaxResultBytes          = 1 << 20 // 1 MiB
	DefaultMaxResultHandles        = 32
	DefaultMaxEventPayloadBytes    = 1 << 20 // 1 MiB
	DefaultMaxCallDepth            = 8
)

// Defaults chosen to be usable for validation and pricing logic — the cases ADR
// 0001 names for this tier — and far below anything that threatens the host.
const (
	DefaultMaxMemoryPages   = 256 // 16 MiB
	DefaultTimeout          = 50 * time.Millisecond
	DefaultMaxResponseBytes = 1 << 20 // 1 MiB
)

func (c *Config) applyDefaults() {
	if c.MaxMemoryPages == 0 {
		c.MaxMemoryPages = DefaultMaxMemoryPages
	}
	if c.Timeout == 0 {
		c.Timeout = DefaultTimeout
	}
	if c.MaxResponseBytes == 0 {
		c.MaxResponseBytes = DefaultMaxResponseBytes
	}
}

// Plugin is a loaded guest.
type Plugin struct {
	config   Config
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
	v2       *v2Deps
}

// Load compiles a guest and verifies it implements the ABI.
//
// Compilation happens once; each invocation instantiates a fresh module. That is
// deliberate and costs something: instantiating per call means a guest cannot
// accumulate state between invocations, which is what stops one tenant's call
// from leaving data another tenant's call can read. A long-lived instance would
// be faster and would make cross-tenant leakage a matter of guest discipline.
func Load(ctx context.Context, config Config) (*Plugin, error) {
	config.applyDefaults()

	if config.ModuleID == "" {
		return nil, fmt.Errorf("wasmhost: module ID is required")
	}
	if len(config.Wasm) == 0 {
		return nil, fmt.Errorf("wasmhost: no module bytes for %s", config.ModuleID)
	}

	runtimeConfig := wazero.NewRuntimeConfig().
		WithMemoryLimitPages(config.MaxMemoryPages).
		// Closing on context done is what makes the timeout real rather than
		// advisory.
		WithCloseOnContextDone(true)

	runtime := wazero.NewRuntimeWithConfig(ctx, runtimeConfig)

	// The minimum WASI a compiled guest needs to boot, with everything that
	// reaches outside the sandbox refused. Deliberately not wazero's WASI
	// implementation: that would hand untrusted code a filesystem, a clock, an
	// environment, and the host's entropy. See wasi.go.
	if err := instantiateWASI(ctx, runtime, config.Stdout, config.Stderr); err != nil {
		_ = runtime.Close(ctx)
		return nil, fmt.Errorf("wasmhost: prepare sandbox for %s: %w", config.ModuleID, err)
	}

	// The v2 host-call module is always wired, even for a guest with no V2
	// grants: an ungranted guest that imports it gets a denial through the
	// ordinary ErrHostCallDenied path rather than a link error that looks like
	// a build problem.
	if err := instantiateV2Host(ctx, runtime); err != nil {
		_ = runtime.Close(ctx)
		return nil, fmt.Errorf("wasmhost: prepare host-call surface for %s: %w", config.ModuleID, err)
	}

	compiled, err := runtime.CompileModule(ctx, config.Wasm)
	if err != nil {
		_ = runtime.Close(ctx)
		return nil, fmt.Errorf("wasmhost: compile %s: %w", config.ModuleID, err)
	}

	// The ABI is checked at load. A guest missing an export fails here, where an
	// operator installing it is looking, rather than on the first request.
	exports := compiled.ExportedFunctions()
	for _, required := range []string{ExportAlloc, ExportHandle} {
		if _, ok := exports[required]; !ok {
			_ = compiled.Close(ctx)
			_ = runtime.Close(ctx)
			return nil, fmt.Errorf("%w: %s exports no %s",
				ErrGuestContract, config.ModuleID, required)
		}
	}

	// A reactor build exports _initialize; a command build exports _start and
	// would run the guest's main and exit before the host could call anything.
	// Refused here rather than surfacing as "notInitialized" on the first alloc.
	if _, ok := exports["_initialize"]; !ok {
		_ = compiled.Close(ctx)
		_ = runtime.Close(ctx)
		return nil, fmt.Errorf("%w: %s exports no _initialize; build it as a reactor "+
			"(for Go: -buildmode=c-shared) rather than a command",
			ErrGuestContract, config.ModuleID)
	}

	var v2 *v2Deps
	if config.V2 != nil {
		v2 = newV2Deps(config.ModuleID, *config.V2)
	}

	return &Plugin{config: config, runtime: runtime, compiled: compiled, v2: v2}, nil
}

// Close releases the runtime.
func (p *Plugin) Close(ctx context.Context) error {
	if err := p.compiled.Close(ctx); err != nil {
		return err
	}
	return p.runtime.Close(ctx)
}

// Invoke calls the guest with an encoded request and returns its encoded
// response.
func (p *Plugin) Invoke(ctx context.Context, request []byte) ([]byte, error) {
	// The caller's deadline is honoured if it is tighter: a guest must not
	// outlive the request it is serving.
	timeout := p.config.Timeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 && remaining < timeout {
			timeout = remaining
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Result handles and the last diagnostic exist only for this one top-level
	// invocation. They are attached to ctx because wazero threads the same Go
	// context through every host function a guest's own call reaches, and are
	// discarded when Invoke returns — they cannot carry data between calls or
	// between tenants.
	if p.v2 != nil {
		ctx = withV2Deps(ctx, p.v2)
		ctx = withCallState(ctx, newCallState())
	}

	// A fresh instance per call. The guest starts from a known state every time,
	// so nothing one tenant's invocation writes can be read by another's.
	//
	// NewModuleConfig grants nothing: no filesystem, no environment, no args, no
	// stdio. Host functions are the entire capability surface, and none are wired
	// yet.
	instance, err := p.runtime.InstantiateModule(ctx, p.compiled,
		wazero.NewModuleConfig().
			WithName("").
			// _initialize, not _start. A plugin is a library, not a program:
			// _initialize sets up the guest's runtime and returns, leaving the
			// exported functions callable. _start would run the guest's main and
			// exit, so the module would be gone before the host called anything.
			//
			// A guest that exports neither is refused at load, where the operator
			// installing it is looking.
			WithStartFunctions("_initialize"))
	if err != nil {
		return nil, p.classify(ctx, fmt.Errorf("instantiate: %w", err))
	}
	defer instance.Close(ctx)

	memory := instance.Memory()
	if memory == nil {
		return nil, fmt.Errorf("%w: %s exports no memory", ErrGuestContract, p.config.ModuleID)
	}

	alloc := instance.ExportedFunction(ExportAlloc)
	handle := instance.ExportedFunction(ExportHandle)

	// The guest allocates, because only its allocator knows what is free. A host
	// that picked an address would corrupt the guest's heap.
	allocated, err := alloc.Call(ctx, uint64(len(request)))
	if err != nil {
		return nil, p.classify(ctx, fmt.Errorf("alloc: %w", err))
	}
	if len(allocated) == 0 {
		return nil, fmt.Errorf("%w: %s alloc returned nothing", ErrGuestContract, p.config.ModuleID)
	}
	ptr := uint32(allocated[0])

	// Write is bounds-checked by wazero against the guest's linear memory, so a
	// guest that returned a bogus pointer cannot make the host write outside the
	// sandbox.
	if !memory.Write(ptr, request) {
		return nil, fmt.Errorf("%w: %s returned an out-of-range pointer",
			ErrGuestFailed, p.config.ModuleID)
	}

	results, err := handle.Call(ctx, uint64(ptr), uint64(len(request)))
	if err != nil {
		return nil, p.classify(ctx, fmt.Errorf("handle: %w", err))
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("%w: %s handler returned nothing",
			ErrGuestContract, p.config.ModuleID)
	}

	responsePtr, responseLen := packResult(results[0])
	if responseLen > p.config.MaxResponseBytes {
		return nil, fmt.Errorf("%w: %s returned %d bytes, limit is %d",
			ErrResponseTooLarge, p.config.ModuleID, responseLen, p.config.MaxResponseBytes)
	}

	response, ok := memory.Read(responsePtr, responseLen)
	if !ok {
		return nil, fmt.Errorf("%w: %s returned an out-of-range response",
			ErrGuestFailed, p.config.ModuleID)
	}

	// Copied out before the instance is closed: the slice points into linear
	// memory that is about to be freed.
	out := make([]byte, len(response))
	copy(out, response)
	return out, nil
}

// classify separates a timeout from any other guest failure.
//
// An operator responds to the two differently: a timeout means the limit is
// wrong or the guest is slow, and anything else means the guest is broken.
func (p *Plugin) classify(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %s exceeded %s", ErrGuestTimeout, p.config.ModuleID, p.config.Timeout)
	}
	return fmt.Errorf("%w: %s: %v", ErrGuestFailed, p.config.ModuleID, err)
}

// Handler adapts a plugin to the kernel's capability handler signature.
//
// This is the whole point of the tier: the registry stores a modulekit.Handler
// and cannot tell that this one enters a sandbox. ADR 0001's claim — the same
// capability lives at any tier, and callers cannot tell the difference — is
// either true here or it is not true at all.
func (p *Plugin) Handler(ref modulekit.CapabilityRef, codec Codec) modulekit.Handler {
	return func(ctx context.Context, request any) (any, error) {
		// The tenant travels in the Go context, which does not cross into the
		// guest. It is re-established as an explicit field in the envelope, so
		// the guest knows which tenant it is acting for — and cannot choose,
		// because the host writes it from context rather than reading it from
		// the request.
		tenantID := tenant.IDFromContext(ctx)
		if tenantID == "" {
			return nil, fmt.Errorf("wasmhost: no tenant in context for %s", ref.Key())
		}

		maxDepth := uint32(DefaultMaxCallDepth)
		if p.v2 != nil {
			maxDepth = p.v2.limits.maxCallDepth
		}
		chain := callChainFrom(ctx)
		if slices.Contains(chain, ref.Key()) {
			return nil, fmt.Errorf("%w: %s", ErrCallCycle, ref.Key())
		}
		if uint32(len(chain)) >= maxDepth {
			return nil, fmt.Errorf("%w: %s would exceed depth %d", ErrCallDepthExceeded, ref.Key(), maxDepth)
		}
		ctx = withCallChain(ctx, append(append([]string(nil), chain...), ref.Key()))

		payload, err := codec.EncodeRequest(ctx, ref, tenantID, request)
		if err != nil {
			return nil, err
		}

		raw, err := p.Invoke(ctx, payload)
		if err != nil {
			return nil, err
		}

		status, body, err := statusOf(raw)
		if err != nil {
			return nil, err
		}
		if status == StatusError {
			// A deliberate rejection by the guest, not a malfunction. Returned as
			// a capability error so a fail-closed hook can tell a veto from a
			// crash — the distinction the status byte exists for.
			return nil, fmt.Errorf("%w: %s", modulekit.ErrForbidden, string(body))
		}

		return codec.DecodeResponse(ref, body)
	}
}
