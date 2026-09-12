package hooks_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sannados/sannad/internal/kernel/hooks"
	"github.com/sannados/sannad/pkg/modulekit"
)

// The tests in this file began as adversarial probes for ways around the hook
// guarantees. Two found real defects — a caller cancellation reported as a hook
// timeout, and a doubly-wrapped failure error — and are kept as regressions.

// An observer must not be able to block a flow by returning a Rejection. The
// kinds only mean something if the dispatcher enforces them.
func TestGuaranteeObserverCannotVeto(t *testing.T) {
	r := hooks.New()
	mustOffer(t, r, "owner", point("obs", modulekit.KindObserver, modulekit.FailOpen))
	mustSubscribe(t, r, modulekit.Subscription{
		Ref: ref("obs"), ModuleID: "sneaky",
		Handler: func(context.Context, any) (any, error) {
			return modulekit.Rejection{Reason: "I am secretly a validator"}, nil
		},
	})
	if err := r.Notify(context.Background(), ref("obs"), "x"); err != nil {
		t.Fatalf("an observer blocked the flow: %v", err)
	}
}

// A Rejection returned from a transformer is an ordinary value, not a veto.
// Only the validator path gives it meaning.
func TestGuaranteeTransformerRejectionIsJustAValue(t *testing.T) {
	r := hooks.New()
	mustOffer(t, r, "owner", point("t", modulekit.KindTransformer, modulekit.FailClosed))
	mustSubscribe(t, r, modulekit.Subscription{
		Ref: ref("t"), ModuleID: "sneaky",
		Handler: func(context.Context, any) (any, error) {
			return modulekit.Rejection{Reason: "veto"}, nil
		},
	})
	got, err := r.Transform(context.Background(), ref("t"), "x")
	if err != nil {
		t.Fatalf("a transformer vetoed: %v", err)
	}
	if _, isRejection := got.(modulekit.Rejection); !isRejection {
		t.Fatalf("expected the value through unchanged, got %T", got)
	}
}

// Concurrent dispatches must not see each other's values. Dispatch copies the
// subscriber slice under the lock rather than iterating shared state.
func TestGuaranteeNoCrossDispatchLeak(t *testing.T) {
	r := hooks.New()
	mustOffer(t, r, "owner", point("t", modulekit.KindTransformer, modulekit.FailClosed))
	mustSubscribe(t, r, modulekit.Subscription{
		Ref: ref("t"), ModuleID: "echo",
		Handler: func(_ context.Context, v any) (any, error) { return v, nil },
	})

	done := make(chan string, 20)
	for i := 0; i < 20; i++ {
		want := string(rune('a' + i))
		go func() {
			got, _ := r.Transform(context.Background(), ref("t"), want)
			if got != want {
				done <- "LEAK: got " + got.(string) + " want " + want
				return
			}
			done <- ""
		}()
	}
	for i := 0; i < 20; i++ {
		if msg := <-done; msg != "" {
			t.Fatal(msg)
		}
	}
}

// A subscriber that times out and finishes later must not have its value
// reach the result. The goroutine cannot be killed, so the guarantee is that
// its output is discarded.
func TestGuaranteeLateSubscriberCannotAffectResult(t *testing.T) {
	r := hooks.New()
	p := point("t", modulekit.KindTransformer, modulekit.FailOpen)
	p.Timeout = 30 * time.Millisecond
	mustOffer(t, r, "owner", p)

	mustSubscribe(t, r, modulekit.Subscription{
		Ref: ref("t"), ModuleID: "slow", Priority: 0,
		Handler: func(_ context.Context, v any) (any, error) {
			time.Sleep(80 * time.Millisecond)
			return "CORRUPTED", nil
		},
	})
	mustSubscribe(t, r, modulekit.Subscription{
		Ref: ref("t"), ModuleID: "fast", Priority: 1,
		Handler: func(_ context.Context, v any) (any, error) { return v, nil },
	})

	got, err := r.Transform(context.Background(), ref("t"), "original")
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	if got != "original" {
		t.Fatalf("a timed-out subscriber's value reached the result: %v", got)
	}
	time.Sleep(100 * time.Millisecond) // let the abandoned goroutine finish
}

// Two subscriptions from one module both run. This is deliberate — a module
// may legitimately want two handlers at different priorities — and is recorded
// here so it is a known property rather than a surprise.
func TestGuaranteeDoubleSubscription(t *testing.T) {
	r := hooks.New()
	mustOffer(t, r, "owner", point("t", modulekit.KindTransformer, modulekit.FailClosed))
	sub := modulekit.Subscription{
		Ref: ref("t"), ModuleID: "twice",
		Handler: func(_ context.Context, v any) (any, error) { return v.(int) + 1, nil },
	}
	mustSubscribe(t, r, sub)
	mustSubscribe(t, r, sub)

	got, _ := r.Transform(context.Background(), ref("t"), 0)
	if got != 2 {
		t.Fatalf("got %v, want 2: both subscriptions should run", got)
	}
}

// Regression: a cancelled caller must not be reported as a hook timeout.
// Both arrive as ctx.Done(), and conflating them sends whoever debugs the
// failure looking for a slow subscriber that was never slow.
func TestGuaranteeParentCancellationPropagates(t *testing.T) {
	r := hooks.New()
	p := point("obs", modulekit.KindObserver, modulekit.FailClosed)
	p.Timeout = 5 * time.Second
	mustOffer(t, r, "owner", p)

	mustSubscribe(t, r, modulekit.Subscription{
		Ref: ref("obs"), ModuleID: "waits",
		Handler: func(ctx context.Context, _ any) (any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()

	start := time.Now()
	err := r.Notify(ctx, ref("obs"), "x")
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Fatal("parent cancellation did not reach the subscriber")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled in the chain, got: %v", err)
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Fatalf("caller cancellation reported as a hook timeout: %v", err)
	}
	// The failure is tagged exactly once; double-wrapping reads as two problems.
	if strings.Count(err.Error(), "hook subscriber failed") != 1 {
		t.Fatalf("error wrapped more than once: %v", err)
	}
}
