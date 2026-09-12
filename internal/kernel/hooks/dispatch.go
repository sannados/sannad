package hooks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sannados/sannad/pkg/modulekit"
)

// RejectedError carries every veto from a validator dispatch.
//
// ADR 0003 decision 3 requires that a caller sees all rejection reasons rather
// than only the first, because fixing one refusal to discover the next is a
// poor experience for a user filling in a form.
type RejectedError struct {
	Ref        modulekit.HookRef
	Rejections []modulekit.Rejection
}

func (e *RejectedError) Error() string {
	reasons := make([]string, len(e.Rejections))
	for i, r := range e.Rejections {
		reasons[i] = r.String()
	}
	return fmt.Sprintf("%s: %s rejected: %s",
		modulekit.ErrHookRejected.Error(), e.Ref.Key(), strings.Join(reasons, "; "))
}

// Unwrap lets callers test with errors.Is(err, modulekit.ErrHookRejected)
// while still type-asserting for the individual reasons.
func (e *RejectedError) Unwrap() error { return modulekit.ErrHookRejected }

// Notify dispatches an observer hook point.
//
// Return values are ignored and, on a fail_open point, so are failures: an
// observer that could block the operation would be a validator wearing the
// wrong label. Subscribers run in dispatch order rather than concurrently, so
// a flow's side effects stay reproducible.
func (r *Registry) Notify(ctx context.Context, ref modulekit.HookRef, value any) error {
	point, subs, ok := r.subscribersOf(ref.Key())
	if !ok {
		return errNoSuchPoint(ref)
	}
	if err := requireKind(point.point, modulekit.KindObserver); err != nil {
		return err
	}

	for _, sub := range subs {
		_, err := invoke(ctx, point.point, sub, value)
		if err := applyPolicy(point.point, sub, err, func(e error) {
			slog.WarnContext(ctx, "hook subscriber failed (fail_open)",
				"hook", ref.Key(), "subscriber", sub.ModuleID, "err", e)
		}); err != nil {
			return err
		}
	}
	return nil
}

// Validate dispatches a validator hook point.
//
// Every subscriber runs even after one has vetoed, so the caller receives the
// complete set of reasons. A returned *RejectedError means the operation was
// refused; any other error means a subscriber broke on a fail_closed point,
// which is a different condition and must not be reported as a business veto.
func (r *Registry) Validate(ctx context.Context, ref modulekit.HookRef, value any) error {
	point, subs, ok := r.subscribersOf(ref.Key())
	if !ok {
		return errNoSuchPoint(ref)
	}
	if err := requireKind(point.point, modulekit.KindValidator); err != nil {
		return err
	}

	var rejections []modulekit.Rejection
	for _, sub := range subs {
		result, err := invoke(ctx, point.point, sub, value)
		if err := applyPolicy(point.point, sub, err, func(e error) {
			slog.WarnContext(ctx, "hook subscriber failed (fail_open)",
				"hook", ref.Key(), "subscriber", sub.ModuleID, "err", e)
		}); err != nil {
			return err
		}
		if rej, ok := asRejection(result); ok {
			// The dispatcher stamps the module ID rather than trusting the
			// subscriber to report its own identity accurately.
			rej.ModuleID = sub.ModuleID
			rejections = append(rejections, rej)
		}
	}

	if len(rejections) > 0 {
		return &RejectedError{Ref: ref, Rejections: rejections}
	}
	return nil
}

// asRejection recognises both the value and pointer forms, since returning
// either is a natural thing for a subscriber to write.
func asRejection(result any) (modulekit.Rejection, bool) {
	switch v := result.(type) {
	case modulekit.Rejection:
		return v, true
	case *modulekit.Rejection:
		if v != nil {
			return *v, true
		}
	}
	return modulekit.Rejection{}, false
}

// Transform dispatches a transformer hook point, threading the value through
// subscribers in priority order.
//
// A subscriber returning nil leaves the value unchanged, so a transformer that
// only wants to act on some inputs does not have to echo the value back.
//
// On a fail_open point a failed subscriber is skipped and the chain continues
// with the last good value, which is why the running value is only replaced
// after a subscriber succeeds.
func (r *Registry) Transform(ctx context.Context, ref modulekit.HookRef, value any) (any, error) {
	point, subs, ok := r.subscribersOf(ref.Key())
	if !ok {
		return nil, errNoSuchPoint(ref)
	}
	if err := requireKind(point.point, modulekit.KindTransformer); err != nil {
		return nil, err
	}

	current := value
	for _, sub := range subs {
		result, err := invoke(ctx, point.point, sub, current)
		if err := applyPolicy(point.point, sub, err, func(e error) {
			slog.WarnContext(ctx, "hook subscriber failed (fail_open), value unchanged",
				"hook", ref.Key(), "subscriber", sub.ModuleID, "err", e)
		}); err != nil {
			return nil, err
		}
		if err == nil && result != nil {
			current = result
		}
	}
	return current, nil
}

// RejectionsFrom extracts the individual vetoes from an error returned by
// Validate, so a caller can render them without depending on this package's
// error formatting.
func RejectionsFrom(err error) ([]modulekit.Rejection, bool) {
	var rejected *RejectedError
	if errors.As(err, &rejected) {
		return rejected.Rejections, true
	}
	return nil, false
}

// Registry implements modulekit.HookDispatcher, so a module can fire the hook
// points it owns through the SDK rather than importing this package. Asserted
// here so a signature drift is a compile error rather than a module that cannot
// dispatch.
var _ modulekit.HookDispatcher = (*Registry)(nil)
