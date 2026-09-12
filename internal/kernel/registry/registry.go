package registry

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/sannados/sannad/internal/kernel/hooks"
	"github.com/sannados/sannad/pkg/modulekit"
)

var ErrDuplicateModule = errors.New("registry: module already registered")
var ErrDuplicateCapability = errors.New("registry: capability already registered")

// ErrInvalidDescriptor is returned when a module's descriptor contradicts
// itself — exposing a capability it does not provide, for instance. Refused at
// registration, where the author is looking, rather than surfacing as a
// confusing 404 at the bridge.
var ErrInvalidDescriptor = errors.New("registry: invalid module descriptor")

type Registry struct {
	mu                sync.RWMutex
	modules           map[string]modulekit.Module
	handlers          map[string]modulekit.Handler
	registrationOrder []string
	hooks             *hooks.Registry
	// installing is the module currently inside Install. Subscribe needs the
	// subscriber's identity, and taking it from the module being installed is
	// what stops a module from subscribing under another module's name.
	installing string

	// exposed is the set of capabilities published outside the process, keyed by
	// ref. Kept separate from handlers because being callable and being public
	// are different questions with different answers.
	exposed map[string]modulekit.CapabilityRef
}

func New() *Registry {
	return &Registry{
		modules:           make(map[string]modulekit.Module),
		handlers:          make(map[string]modulekit.Handler),
		registrationOrder: make([]string, 0),
		hooks:             hooks.New(),
		exposed:           make(map[string]modulekit.CapabilityRef),
	}
}

func (r *Registry) RegisterModule(item modulekit.Module) error {
	descriptor := item.Descriptor()

	// Refused here rather than defaulted. A module that never declared where it
	// keeps its data is exactly the one whose author did not consider tenant
	// isolation, and silently assuming the safe answer would hide it.
	if !descriptor.Storage.Valid() {
		return fmt.Errorf("%w: module %s must declare Storage as %q, %q, or %q",
			ErrInvalidDescriptor, descriptor.ID,
			modulekit.StorageKernel, modulekit.StorageOwn, modulekit.StorageNone)
	}

	// Refused at registration rather than discovered when a caller reaches the
	// bridge and gets a confusing 404. A module exposing something it does not
	// serve is a descriptor mistake, and the cheapest place to say so is here.
	provides := make(map[string]struct{}, len(descriptor.Provides))
	for _, ref := range descriptor.Provides {
		provides[ref.Key()] = struct{}{}
	}
	for _, ref := range descriptor.Exposed {
		if _, ok := provides[ref.Key()]; !ok {
			return fmt.Errorf("%w: module %s exposes %s but does not provide it",
				ErrInvalidDescriptor, descriptor.ID, ref.Key())
		}
	}

	r.mu.Lock()
	if _, exists := r.modules[descriptor.ID]; exists {
		r.mu.Unlock()
		return ErrDuplicateModule
	}

	r.modules[descriptor.ID] = item
	r.installing = descriptor.ID
	for _, ref := range descriptor.Exposed {
		r.exposed[ref.Key()] = ref
	}
	r.mu.Unlock()

	// Hook points are offered before Install so a module can subscribe to its
	// own points, and so a later module's subscription finds them already
	// declared.
	installErr := func() error {
		for _, point := range descriptor.Offers {
			if err := r.hooks.Offer(descriptor.ID, point); err != nil {
				return err
			}
		}
		return item.Install(r)
	}()

	r.mu.Lock()
	r.installing = ""
	r.mu.Unlock()

	if installErr != nil {
		r.mu.Lock()
		delete(r.modules, descriptor.ID)
		for _, capability := range descriptor.Provides {
			delete(r.handlers, capability.Key())
		}
		for _, capability := range descriptor.Exposed {
			delete(r.exposed, capability.Key())
		}
		r.mu.Unlock()
		return installErr
	}

	r.mu.Lock()
	r.registrationOrder = append(r.registrationOrder, descriptor.ID)
	r.mu.Unlock()

	return nil
}

func (r *Registry) RegisterCapability(ref modulekit.CapabilityRef, handler modulekit.Handler) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := ref.Key()
	if _, exists := r.handlers[key]; exists {
		return ErrDuplicateCapability
	}

	r.handlers[key] = handler
	return nil
}

func (r *Registry) ResolveCapability(ref modulekit.CapabilityRef) (modulekit.Handler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	handler, exists := r.handlers[ref.Key()]
	return handler, exists
}

// validateConsumes checks that every capability declared in a module's
// Consumes list is registered before StartAll proceeds. This catches
// wiring errors at boot rather than at runtime.
func (r *Registry) validateConsumes() error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var errs []error
	for _, id := range r.registrationOrder {
		m := r.modules[id]
		for _, ref := range m.Descriptor().Consumes {
			if _, ok := r.handlers[ref.Key()]; !ok {
				errs = append(errs, fmt.Errorf("registry: module %s consumes %s which is not registered", id, ref.Key()))
			}
		}
	}
	return errors.Join(errs...)
}

// IsExposed reports whether a capability was published for callers outside the
// process.
//
// The bridge asks this before dispatching. Resolving a capability is not enough:
// registration means other modules may call it, and publication is a separate,
// deliberate decision by the module's author.
func (r *Registry) IsExposed(ref modulekit.CapabilityRef) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.exposed[ref.Key()]
	return ok
}

// ListExposed returns the capabilities callable from outside the process,
// sorted. This is what an external integrator may discover; ListCapabilities
// includes internal ones and is not safe to publish.
func (r *Registry) ListExposed() []modulekit.CapabilityRef {
	r.mu.RLock()
	defer r.mu.RUnlock()

	refs := make([]modulekit.CapabilityRef, 0, len(r.exposed))
	for _, ref := range r.exposed {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Key() < refs[j].Key() })
	return refs
}

// SelfManagedStorage lists the modules that keep their own data.
//
// The startup report reads this. These modules are the isolation surface an
// operator is trusting rather than one the kernel enforces, and the audit that
// inspects database tables cannot see them at all.
func (r *Registry) SelfManagedStorage() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var ids []string
	for id, item := range r.modules {
		if item.Descriptor().Storage == modulekit.StorageOwn {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (r *Registry) ListCapabilities() []modulekit.CapabilityRef {
	r.mu.RLock()
	defer r.mu.RUnlock()

	capabilities := make([]modulekit.CapabilityRef, 0)
	for _, item := range r.modules {
		capabilities = append(capabilities, item.Descriptor().Provides...)
	}

	sort.Slice(capabilities, func(i, j int) bool {
		return capabilities[i].Key() < capabilities[j].Key()
	})

	return capabilities
}

// StartAll calls Start on every registered module in registration order.
// If any module fails to start, already-started modules are stopped in
// reverse order before the error is returned.
func (r *Registry) StartAll(ctx context.Context) error {
	if err := r.validateConsumes(); err != nil {
		return err
	}

	r.mu.RLock()
	order := make([]string, len(r.registrationOrder))
	copy(order, r.registrationOrder)
	r.mu.RUnlock()

	started := make([]string, 0, len(order))
	for _, id := range order {
		r.mu.RLock()
		m := r.modules[id]
		r.mu.RUnlock()

		if err := m.Start(ctx); err != nil {
			_ = r.stopModules(ctx, started)
			return fmt.Errorf("registry: module %s failed to start: %w", id, err)
		}
		started = append(started, id)
	}
	return nil
}

// StopAll calls Stop on every registered module in reverse registration order.
// All modules are stopped even if some return errors; errors are joined.
func (r *Registry) StopAll(ctx context.Context) error {
	r.mu.RLock()
	order := make([]string, len(r.registrationOrder))
	copy(order, r.registrationOrder)
	r.mu.RUnlock()

	return r.stopModules(ctx, order)
}

func (r *Registry) stopModules(ctx context.Context, ids []string) error {
	var errs []error
	for i := len(ids) - 1; i >= 0; i-- {
		r.mu.RLock()
		m := r.modules[ids[i]]
		r.mu.RUnlock()

		if err := m.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("registry: module %s failed to stop: %w", ids[i], err))
		}
	}
	return errors.Join(errs...)
}

// OfferHook implements modulekit.HookRegistrar. A module normally declares its
// hook points in its Descriptor; this exists for points computed at install
// time.
func (r *Registry) OfferHook(point modulekit.HookPoint) error {
	r.mu.RLock()
	owner := r.installing
	r.mu.RUnlock()
	if owner == "" {
		return fmt.Errorf("%w: OfferHook called outside module installation", modulekit.ErrInvalidHookPoint)
	}
	return r.hooks.Offer(owner, point)
}

// Subscribe implements modulekit.HookRegistrar.
//
// The subscriber's module ID is taken from the module being installed, not from
// the Subscription. A module cannot subscribe under another module's name, so
// the attribution in a rejection or a failure log is trustworthy.
func (r *Registry) Subscribe(sub modulekit.Subscription) error {
	r.mu.RLock()
	subscriber := r.installing
	r.mu.RUnlock()
	if subscriber == "" {
		return fmt.Errorf("%w: Subscribe called outside module installation", modulekit.ErrInvalidHookPoint)
	}
	sub.ModuleID = subscriber
	return r.hooks.Subscribe(sub)
}

// Hooks returns the hook registry for dispatch.
func (r *Registry) Hooks() *hooks.Registry { return r.hooks }

// Notify, Validate, and Transform forward to the hook registry so a module can
// fire the hook points it owns through the Registrar it already holds.
//
// Without these a module could declare a hook point and never dispatch it,
// because dispatch lives in an internal package a community module may not
// import. The offer would compile, subscribers would attach, and the hook would
// simply never fire — the failure being silence rather than an error.
func (r *Registry) Notify(ctx context.Context, ref modulekit.HookRef, value any) error {
	return r.hooks.Notify(ctx, ref, value)
}

func (r *Registry) Validate(ctx context.Context, ref modulekit.HookRef, value any) error {
	return r.hooks.Validate(ctx, ref, value)
}

func (r *Registry) Transform(ctx context.Context, ref modulekit.HookRef, value any) (any, error) {
	return r.hooks.Transform(ctx, ref, value)
}

// OfferedPoints forwards to the hook registry so a module installing itself
// can check the kind of a point it wants to subscribe to before asking to
// subscribe, and report a clear mismatch rather than a runtime dispatch
// surprise. Used by the WASM tier, whose manifest states an expected kind
// separately from the subscription itself.
func (r *Registry) OfferedPoints() []modulekit.HookPoint {
	return r.hooks.OfferedPoints()
}

// Asserted so a module can rely on HookDispatcherFrom succeeding.
var _ modulekit.HookDispatcher = (*Registry)(nil)
