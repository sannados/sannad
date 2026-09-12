package modulekit

import (
	"context"
	"fmt"
	"time"
)

// HookKind determines what the kernel does with a subscriber's return value.
//
// Mixing these is a common design failure: an observer that can secretly block,
// or a transformer with unclear ordering, produces a system nobody can reason
// about. The kind is therefore a property of the hook point, declared by the
// module that offers it, and the dispatcher enforces it. See ADR 0003.
type HookKind string

const (
	// KindObserver notifies subscribers after the fact. Return values are
	// ignored and a subscriber cannot block the operation.
	KindObserver HookKind = "observer"

	// KindValidator lets a subscriber veto. Every validator runs and every
	// rejection is collected, so a caller sees all reasons rather than only
	// the first.
	KindValidator HookKind = "validator"

	// KindTransformer passes a value through subscribers in priority order,
	// each receiving the previous one's output.
	KindTransformer HookKind = "transformer"
)

func (k HookKind) Valid() bool {
	switch k {
	case KindObserver, KindValidator, KindTransformer:
		return true
	}
	return false
}

// FailurePolicy states what happens when a subscriber fails or times out.
// It is declared on the hook point, never inferred, so the blast radius of a
// third-party failure is a decision the flow owner made deliberately.
type FailurePolicy string

const (
	// FailClosed aborts the operation when a subscriber fails. For
	// correctness-critical hooks such as credit checks.
	FailClosed FailurePolicy = "fail_closed"

	// FailOpen logs and skips a failed subscriber. For enrichment and
	// non-essential logic.
	FailOpen FailurePolicy = "fail_open"
)

func (p FailurePolicy) Valid() bool {
	return p == FailClosed || p == FailOpen
}

// HookRef identifies a hook point. Offering one is a public API commitment,
// versioned like any other contract.
type HookRef struct {
	Name    string
	Version string
}

func (ref HookRef) Key() string {
	return ref.Name + "@" + ref.Version
}

// HookPoint is the declaration a module makes when it opens a flow to
// participation by modules it does not know about.
type HookPoint struct {
	Ref  HookRef
	Kind HookKind

	// Policy applies to subscriber failure and timeout. Required.
	Policy FailurePolicy

	// Timeout bounds every synchronous subscriber invocation. Required and
	// must be positive: there is no unbounded wait on third-party code, in
	// process or remote.
	Timeout time.Duration

	// Description is developer-facing documentation for subscribers.
	Description string
}

// Validate reports whether the declaration is usable. A malformed hook point is
// refused at registration rather than discovered when a subscriber misbehaves.
func (h HookPoint) Validate() error {
	if h.Ref.Name == "" || h.Ref.Version == "" {
		return fmt.Errorf("%w: hook point needs both a name and a version", ErrInvalidHookPoint)
	}
	if !h.Kind.Valid() {
		return fmt.Errorf("%w: unknown hook kind %q for %s", ErrInvalidHookPoint, h.Kind, h.Ref.Key())
	}
	if !h.Policy.Valid() {
		return fmt.Errorf("%w: hook point %s must declare fail_open or fail_closed, got %q",
			ErrInvalidHookPoint, h.Ref.Key(), h.Policy)
	}
	if h.Timeout <= 0 {
		return fmt.Errorf("%w: hook point %s must declare a positive timeout",
			ErrInvalidHookPoint, h.Ref.Key())
	}
	return nil
}

// HookHandler is a subscriber's implementation.
//
// What it returns depends on the hook kind:
//   - observer:    return value ignored; return nil.
//   - validator:   return a Rejection to veto, or nil to accept.
//   - transformer: return the (possibly modified) value; returning nil keeps
//     the input unchanged.
//
// Like Handler, this is the in-process, same-binary form. Cross-tier
// subscribers are defined by a protobuf message pair; see HandleTypedHook.
type HookHandler func(ctx context.Context, value any) (any, error)

// Subscription is a module's request to participate in a hook point it does
// not own.
type Subscription struct {
	Ref HookRef

	// ModuleID identifies the subscriber. It breaks priority ties, so ordering
	// is deterministic rather than dependent on registration or map iteration.
	ModuleID string

	// Priority orders subscribers, lowest first.
	Priority int

	Handler HookHandler
}

// Rejection is a validator's veto. Returning one is not an error: a refused
// operation is an ordinary outcome, distinct from a subscriber that broke.
type Rejection struct {
	// ModuleID is filled in by the dispatcher, so a caller always knows which
	// subscriber refused without the subscriber having to report it honestly.
	ModuleID string
	Reason   string
}

func (r Rejection) String() string {
	return r.ModuleID + ": " + r.Reason
}

// HookRegistrar is the surface a module uses to offer hook points and to
// subscribe to hook points offered by others.
type HookRegistrar interface {
	// OfferHook declares a hook point this module owns.
	OfferHook(point HookPoint) error

	// Subscribe registers participation in a hook point. Subscribing to a hook
	// point nobody offers is an error, so a typo does not become silence.
	Subscribe(sub Subscription) error
}

// HookDispatcher is the surface a module uses to fire the hook points it owns.
//
// Offering a hook point and firing it are separate interfaces because they are
// separate privileges: any module may offer, but only the owner of a flow fires
// it, and a module that could fire another's hook point could forge the flow.
//
// This is deliberately narrow and mirrors the three kinds a HookPoint may
// declare. There is no general "dispatch" call, because the kind determines what
// a subscriber's return value means, and a caller that picked the wrong one
// would get a silently different contract.
type HookDispatcher interface {
	// Notify runs observers. Their failures do not affect the caller's flow —
	// an observer exists to watch, and a watcher that breaks must not break what
	// it was watching.
	Notify(ctx context.Context, ref HookRef, value any) error

	// Validate runs validators, returning an error carrying every rejection if
	// any subscriber vetoed. Every rejection is reported, not only the first,
	// because a caller fixing one refusal should learn about the rest now.
	Validate(ctx context.Context, ref HookRef, value any) error

	// Transform runs transformers in priority order, threading each result into
	// the next, and returns the final value.
	Transform(ctx context.Context, ref HookRef, value any) (any, error)
}

// HookDispatcherFrom returns the dispatch surface of a Registrar, if it has one.
//
// It mirrors HookRegistrarFrom: Registrar stays narrow so existing modules and
// test doubles keep compiling, and a module that fires hooks asks for the
// surface and handles its absence rather than every implementation growing three
// more methods it does not use.
func HookDispatcherFrom(reg Registrar) (HookDispatcher, bool) {
	d, ok := reg.(HookDispatcher)
	return d, ok
}

// ValidateTyped fires a validator hook point with a concrete value type.
//
// The generic parameter is not for the dispatcher — it carries the value as any
// either way — but for the caller: it pins what this hook point carries at the
// call site, so a later change to the payload type is a compile error here
// rather than an ErrTypeMismatch in somebody else's subscriber at runtime.
func ValidateTyped[T any](ctx context.Context, d HookDispatcher, ref HookRef, value T) error {
	return d.Validate(ctx, ref, value)
}

// NotifyTyped fires an observer hook point with a concrete value type.
func NotifyTyped[T any](ctx context.Context, d HookDispatcher, ref HookRef, value T) error {
	return d.Notify(ctx, ref, value)
}

// TransformTypedValue fires a transformer hook point and returns the result
// coerced back to the caller's type.
//
// Subscribers hand back any, so without this the caller writes a type assertion
// at every call site — and a wrong one is a panic rather than an error.
func TransformTypedValue[T any](ctx context.Context, d HookDispatcher, ref HookRef, value T) (T, error) {
	out, err := d.Transform(ctx, ref, value)
	if err != nil {
		var zero T
		return zero, err
	}
	return coerce[T](out)
}
