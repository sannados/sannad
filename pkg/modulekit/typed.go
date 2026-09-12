package modulekit

import (
	"context"
	"encoding/json"
	"fmt"
)

// HandleTyped registers a capability whose handler takes and returns concrete
// types instead of any.
//
// Every untyped handler begins with a type assertion that can fail at runtime,
// which is precisely the class of error a Go system should eliminate. The
// wrapper performs the assertion once, in one place, with a consistent error.
//
//	modulekit.HandleTyped(reg, ref, func(ctx context.Context, req GetContactRequest) (GetContactResponse, error) {
//	    // no type assertion, no marshalling by hand
//	})
//
// In-process calls pass the typed value directly and skip serialization
// entirely; only cross-tier calls pay the marshalling cost. That is what lets
// one contract be served by the embedded, WASM, and external tiers. See ADR 0003.
func HandleTyped[Req, Resp any](reg Registrar, ref CapabilityRef, fn func(context.Context, Req) (Resp, error)) error {
	return reg.RegisterCapability(ref, func(ctx context.Context, request any) (any, error) {
		req, err := coerce[Req](request)
		if err != nil {
			return nil, fmt.Errorf("capability %s: %w", ref.Key(), err)
		}
		return fn(ctx, req)
	})
}

// RawPayload is an encoded request or response that has not been decoded into
// its contract type yet. A transport — the external bridge, a WASM host — wraps
// a request body in it before dispatch, and the typed handler decodes it.
//
// It is an explicit marker type rather than a sniff for map[string]any because
// the difference between "a module passed a map" and "a transport passed an
// encoded body" cannot be recovered after the fact. Guessing would silently
// reinterpret a module that legitimately passes a map, and a dispatch boundary
// is exactly where a silent reinterpretation becomes a security question.
//
// Modules do not construct these. Transports do.
type RawPayload []byte

// Caller is the dispatch surface a module uses to call another module's
// capability. The kernel bus satisfies it.
//
// It is an interface rather than a concrete type because a module must not name
// the kernel: pkg/modulekit is the whole SDK surface, and internal/kernel/bus is
// not part of it. It also makes a test double one struct rather than a running
// kernel.
type Caller interface {
	Call(ctx context.Context, ref CapabilityRef, request any) (any, error)
}

// CallTyped calls a capability with concrete request and response types.
//
// Without this a caller writes the assertion the provider was spared:
//
//	result, err := bus.Call(ctx, ref, req)
//	resp, ok := result.(ListResponse)   // fails at runtime, not compile time
//
// Two Go modules should not lose type checking to speak through a registry. The
// registry itself must stay untyped — it holds capabilities whose types it
// cannot know — but that is an implementation detail of the registry, not a tax
// on every call site.
//
// The types must come from a package both the provider and the caller import,
// not from the provider's own package. A caller that imports the provider to
// name its request type has a compile-time dependency on the implementation,
// which is exactly what calling by reference exists to avoid: the provider could
// then never be replaced, nor moved to WASM or an external app.
func CallTyped[Req, Resp any](ctx context.Context, caller Caller, ref CapabilityRef, req Req) (Resp, error) {
	var zero Resp

	result, err := caller.Call(ctx, ref, req)
	if err != nil {
		return zero, err
	}

	resp, err := coerce[Resp](result)
	if err != nil {
		// Names the capability, because a mismatch here is a disagreement
		// between two modules about a shared contract, and the error is useless
		// without saying which contract.
		return zero, fmt.Errorf("capability %s response: %w", ref.Key(), err)
	}
	return resp, nil
}

// SubscribeTyped registers a hook subscriber with a concrete value type.
//
// The kind determines what the returned value means — see HookHandler. A
// subscriber whose type does not match the value the hook point carries fails
// with ErrTypeMismatch, which is a wiring error and is reported as such rather
// than being silently skipped.
func SubscribeTyped[T any](reg HookRegistrar, ref HookRef, moduleID string, priority int, fn func(context.Context, T) (any, error)) error {
	return reg.Subscribe(Subscription{
		Ref:      ref,
		ModuleID: moduleID,
		Priority: priority,
		Handler: func(ctx context.Context, value any) (any, error) {
			typed, err := coerce[T](value)
			if err != nil {
				return nil, fmt.Errorf("hook %s subscriber %s: %w", ref.Key(), moduleID, err)
			}
			return fn(ctx, typed)
		},
	})
}

// TransformTyped is the transformer form of SubscribeTyped, where the
// subscriber returns the same type it received. It exists because a transformer
// returning any would put the type assertion back in the caller's code, which
// is what these wrappers remove.
func TransformTyped[T any](reg HookRegistrar, ref HookRef, moduleID string, priority int, fn func(context.Context, T) (T, error)) error {
	return SubscribeTyped(reg, ref, moduleID, priority, func(ctx context.Context, value T) (any, error) {
		return fn(ctx, value)
	})
}

// coerce converts an untyped value to T, accepting both the value and pointer
// forms so a caller passing &req to a handler declared over req still works.
func coerce[T any](value any) (T, error) {
	if typed, ok := value.(T); ok {
		return typed, nil
	}
	if ptr, ok := value.(*T); ok && ptr != nil {
		return *ptr, nil
	}

	// A payload that crossed a transport is encoded, not typed. Decoding it here
	// is what lets one handler serve a caller in this process and a caller on the
	// far side of a network without the module knowing which it is — the ADR 0001
	// claim that the same capability lives at any tier.
	//
	// This runs last, after both assertions, so the in-process path still hands
	// the value over directly and pays nothing.
	if raw, ok := value.(RawPayload); ok {
		var decoded T
		if err := json.Unmarshal(raw, &decoded); err != nil {
			var zero T
			return zero, fmt.Errorf("%w: decoding into %T: %v", ErrTypeMismatch, zero, err)
		}
		return decoded, nil
	}

	var zero T
	return zero, fmt.Errorf("%w: got %T, want %T", ErrTypeMismatch, value, zero)
}
