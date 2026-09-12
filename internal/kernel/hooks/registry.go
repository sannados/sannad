// Package hooks implements the hook registry and dispatcher from ADR 0003.
//
// Where a capability has exactly one provider, a hook point has many
// subscribers. This package owns the ordering, failure-policy, and timeout
// guarantees that make third-party participation in a flow safe for the module
// that owns the flow.
package hooks

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/sannados/sannad/pkg/modulekit"
)

// Registry holds hook points and their subscribers.
//
// Registration happens during module Install, which is single-threaded, but
// dispatch happens on request goroutines, so access is guarded.
type Registry struct {
	mu     sync.RWMutex
	points map[string]offeredPoint
	subs   map[string][]modulekit.Subscription
}

type offeredPoint struct {
	point modulekit.HookPoint
	owner string
}

func New() *Registry {
	return &Registry{
		points: make(map[string]offeredPoint),
		subs:   make(map[string][]modulekit.Subscription),
	}
}

// Offer declares a hook point owned by ownerModuleID.
func (r *Registry) Offer(ownerModuleID string, point modulekit.HookPoint) error {
	if err := point.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	key := point.Ref.Key()
	if existing, ok := r.points[key]; ok {
		return fmt.Errorf("%w: %s offered by both %s and %s",
			modulekit.ErrHookAlreadyOffered, key, existing.owner, ownerModuleID)
	}
	r.points[key] = offeredPoint{point: point, owner: ownerModuleID}
	return nil
}

// Subscribe registers participation in an offered hook point.
//
// The hook point must already be offered. That makes module registration order
// significant, which is the deliberate trade: a subscription to a misspelled or
// absent hook fails at wiring time instead of silently never firing.
func (r *Registry) Subscribe(sub modulekit.Subscription) error {
	if sub.Handler == nil {
		return fmt.Errorf("%w: subscription to %s has no handler",
			modulekit.ErrInvalidHookPoint, sub.Ref.Key())
	}
	if sub.ModuleID == "" {
		return fmt.Errorf("%w: subscription to %s has no module ID",
			modulekit.ErrInvalidHookPoint, sub.Ref.Key())
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	key := sub.Ref.Key()
	if _, ok := r.points[key]; !ok {
		return fmt.Errorf("%w: %s (subscriber %s)",
			modulekit.ErrHookNotOffered, key, sub.ModuleID)
	}
	r.subs[key] = append(r.subs[key], sub)
	return nil
}

// subscribersOf returns the subscribers for a hook point in dispatch order:
// priority ascending, ties broken by module ID.
//
// Sorting on read rather than insert keeps ordering independent of registration
// order — the property ADR 0003 decision 3 requires. The returned slice is a
// copy, so dispatch never holds the lock while running third-party code.
func (r *Registry) subscribersOf(key string) (offeredPoint, []modulekit.Subscription, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	point, ok := r.points[key]
	if !ok {
		return offeredPoint{}, nil, false
	}
	ordered := make([]modulekit.Subscription, len(r.subs[key]))
	copy(ordered, r.subs[key])
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Priority != ordered[j].Priority {
			return ordered[i].Priority < ordered[j].Priority
		}
		return ordered[i].ModuleID < ordered[j].ModuleID
	})
	return point, ordered, true
}

// OfferedPoints returns every declared hook point, for diagnostics.
func (r *Registry) OfferedPoints() []modulekit.HookPoint {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]modulekit.HookPoint, 0, len(r.points))
	for _, p := range r.points {
		out = append(out, p.point)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref.Key() < out[j].Ref.Key() })
	return out
}

// SubscriberIDs returns the module IDs subscribed to a hook point, in dispatch
// order. Answers "who participates in this flow" without running it.
func (r *Registry) SubscriberIDs(ref modulekit.HookRef) []string {
	_, subs, ok := r.subscribersOf(ref.Key())
	if !ok {
		return nil
	}
	ids := make([]string, len(subs))
	for i, s := range subs {
		ids[i] = s.ModuleID
	}
	return ids
}

// errNoSuchPoint is returned when dispatching a hook point nobody offered.
func errNoSuchPoint(ref modulekit.HookRef) error {
	return fmt.Errorf("%w: %s", modulekit.ErrHookNotOffered, ref.Key())
}

// requireKind guards against dispatching a hook point as the wrong kind. The
// kind determines what happens to return values, so calling a validator through
// the observer path would silently discard vetoes.
func requireKind(p modulekit.HookPoint, want modulekit.HookKind) error {
	if p.Kind != want {
		return fmt.Errorf("%w: %s is a %s hook, dispatched as %s",
			modulekit.ErrInvalidHookPoint, p.Ref.Key(), p.Kind, want)
	}
	return nil
}

// invoke runs one subscriber under the hook point's timeout, converting a panic
// into an error. A third-party panic must not take down the host process, and
// an unbounded subscriber must not hold a request open forever.
func invoke(parent context.Context, point modulekit.HookPoint, sub modulekit.Subscription, value any) (result any, err error) {
	ctx, cancel := context.WithTimeout(parent, point.Timeout)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("%w: %s panicked: %v", modulekit.ErrHookSubscriberFailed, sub.ModuleID, p)
			}
			close(done)
		}()
		result, err = sub.Handler(ctx, value)
	}()

	select {
	case <-done:
		return result, err
	case <-ctx.Done():
		// The subscriber goroutine is abandoned, not killed: Go cannot preempt
		// it. The timeout bounds the caller's wait, not the subscriber's life.
		// A subscriber that ignores ctx leaks a goroutine, which is the cost of
		// not being able to kill one.
		//
		// Distinguish the two ways this context ends. Both arrive as ctx.Done(),
		// but reporting a caller cancellation as a hook timeout sends whoever
		// debugs it looking for a slow subscriber that was never slow.
		if parent.Err() != nil {
			return nil, fmt.Errorf("%w: cancelled while running %s: %w",
				modulekit.ErrHookSubscriberFailed, sub.ModuleID, parent.Err())
		}
		return nil, fmt.Errorf("%w: %s timed out after %s",
			modulekit.ErrHookSubscriberFailed, sub.ModuleID, point.Timeout)
	}
}

// applyPolicy decides whether a subscriber failure stops the dispatch.
func applyPolicy(point modulekit.HookPoint, sub modulekit.Subscription, err error, report func(error)) error {
	if err == nil {
		return nil
	}
	// invoke already tags timeouts, cancellations, and panics as subscriber
	// failures. Wrapping those a second time produces an error that says
	// "hook subscriber failed" twice and reads as two separate problems.
	wrapped := err
	if !errors.Is(err, modulekit.ErrHookSubscriberFailed) {
		wrapped = fmt.Errorf("%w: hook %s subscriber %s: %w",
			modulekit.ErrHookSubscriberFailed, point.Ref.Key(), sub.ModuleID, err)
	}
	if point.Policy == modulekit.FailClosed {
		return wrapped
	}
	report(wrapped)
	return nil
}
