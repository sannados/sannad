package hooks_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sannados/sannad/internal/kernel/hooks"
	"github.com/sannados/sannad/pkg/modulekit"
)

func ref(name string) modulekit.HookRef {
	return modulekit.HookRef{Name: name, Version: "v1"}
}

func point(name string, kind modulekit.HookKind, policy modulekit.FailurePolicy) modulekit.HookPoint {
	return modulekit.HookPoint{
		Ref:     ref(name),
		Kind:    kind,
		Policy:  policy,
		Timeout: time.Second,
	}
}

func mustOffer(t *testing.T, r *hooks.Registry, owner string, p modulekit.HookPoint) {
	t.Helper()
	if err := r.Offer(owner, p); err != nil {
		t.Fatalf("Offer %s: %v", p.Ref.Key(), err)
	}
}

func mustSubscribe(t *testing.T, r *hooks.Registry, sub modulekit.Subscription) {
	t.Helper()
	if err := r.Subscribe(sub); err != nil {
		t.Fatalf("Subscribe %s/%s: %v", sub.Ref.Key(), sub.ModuleID, err)
	}
}

// TestMalformedHookPointsAreRefused covers ADR 0003 decision 4: policy and
// timeout are declared, never implied. A hook point that omits either is
// refused at registration rather than discovered when a subscriber misbehaves.
func TestMalformedHookPointsAreRefused(t *testing.T) {
	cases := map[string]modulekit.HookPoint{
		"no name":    {Ref: modulekit.HookRef{Version: "v1"}, Kind: modulekit.KindObserver, Policy: modulekit.FailOpen, Timeout: time.Second},
		"no version": {Ref: modulekit.HookRef{Name: "x"}, Kind: modulekit.KindObserver, Policy: modulekit.FailOpen, Timeout: time.Second},
		"bad kind":   {Ref: ref("x"), Kind: "sideways", Policy: modulekit.FailOpen, Timeout: time.Second},
		"no policy":  {Ref: ref("x"), Kind: modulekit.KindObserver, Timeout: time.Second},
		"no timeout": {Ref: ref("x"), Kind: modulekit.KindObserver, Policy: modulekit.FailOpen},
		"negative timeout": {Ref: ref("x"), Kind: modulekit.KindObserver, Policy: modulekit.FailOpen,
			Timeout: -time.Second},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			r := hooks.New()
			if err := r.Offer("mod", p); !errors.Is(err, modulekit.ErrInvalidHookPoint) {
				t.Fatalf("expected ErrInvalidHookPoint, got %v", err)
			}
		})
	}
}

// TestOneOwnerPerHookPoint: two owners means two contracts behind one name.
func TestOneOwnerPerHookPoint(t *testing.T) {
	r := hooks.New()
	mustOffer(t, r, "mod-a", point("thing", modulekit.KindObserver, modulekit.FailOpen))

	err := r.Offer("mod-b", point("thing", modulekit.KindObserver, modulekit.FailOpen))
	if !errors.Is(err, modulekit.ErrHookAlreadyOffered) {
		t.Fatalf("expected ErrHookAlreadyOffered, got %v", err)
	}
	// The error names both claimants, so the conflict is actionable.
	if !strings.Contains(err.Error(), "mod-a") || !strings.Contains(err.Error(), "mod-b") {
		t.Errorf("error should name both owners, got: %v", err)
	}
}

// TestSubscribingToAnUnofferedHookFails is the anti-silence guarantee: a typo
// in a hook name must fail at wiring time, not become a hook that never fires.
func TestSubscribingToAnUnofferedHookFails(t *testing.T) {
	r := hooks.New()
	err := r.Subscribe(modulekit.Subscription{
		Ref:      ref("nobody.offers.this"),
		ModuleID: "mod",
		Handler:  func(context.Context, any) (any, error) { return nil, nil },
	})
	if !errors.Is(err, modulekit.ErrHookNotOffered) {
		t.Fatalf("expected ErrHookNotOffered, got %v", err)
	}
}

// TestOrderingIsDeterministic covers ADR 0003 decision 3. Registration order
// must not affect dispatch order, so the suite registers the same subscribers
// in two different orders and requires the same result.
func TestOrderingIsDeterministic(t *testing.T) {
	build := func(reverse bool) []string {
		r := hooks.New()
		_ = r.Offer("owner", point("chain", modulekit.KindTransformer, modulekit.FailClosed))

		var mu sync.Mutex
		var order []string
		record := func(id string) modulekit.HookHandler {
			return func(_ context.Context, v any) (any, error) {
				mu.Lock()
				order = append(order, id)
				mu.Unlock()
				return v, nil
			}
		}
		subs := []modulekit.Subscription{
			{Ref: ref("chain"), ModuleID: "zebra", Priority: 10, Handler: record("zebra")},
			{Ref: ref("chain"), ModuleID: "alpha", Priority: 10, Handler: record("alpha")},
			{Ref: ref("chain"), ModuleID: "first", Priority: -5, Handler: record("first")},
			{Ref: ref("chain"), ModuleID: "last", Priority: 99, Handler: record("last")},
		}
		if reverse {
			for i, j := 0, len(subs)-1; i < j; i, j = i+1, j-1 {
				subs[i], subs[j] = subs[j], subs[i]
			}
		}
		for _, s := range subs {
			_ = r.Subscribe(s)
		}
		_, _ = r.Transform(context.Background(), ref("chain"), "value")
		return order
	}

	forward, reversed := build(false), build(true)
	want := []string{"first", "alpha", "zebra", "last"} // priority asc, ties by module ID
	for _, got := range [][]string{forward, reversed} {
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	}
}

// TestTransformerChainsValues confirms each subscriber receives the previous
// one's output rather than the original input.
func TestTransformerChainsValues(t *testing.T) {
	r := hooks.New()
	mustOffer(t, r, "owner", point("price", modulekit.KindTransformer, modulekit.FailClosed))

	for i, id := range []string{"add-ten", "double"} {
		op := id
		mustSubscribe(t, r, modulekit.Subscription{
			Ref: ref("price"), ModuleID: id, Priority: i,
			Handler: func(_ context.Context, v any) (any, error) {
				n := v.(int)
				if op == "add-ten" {
					return n + 10, nil
				}
				return n * 2, nil
			},
		})
	}

	got, err := r.Transform(context.Background(), ref("price"), 5)
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	if got != 30 { // (5+10)*2, not 5*2 or 5+10
		t.Fatalf("got %v, want 30 — subscribers did not chain", got)
	}
}

// TestTransformerNilKeepsValue lets a subscriber act on only some inputs
// without having to echo the value back.
func TestTransformerNilKeepsValue(t *testing.T) {
	r := hooks.New()
	mustOffer(t, r, "owner", point("t", modulekit.KindTransformer, modulekit.FailClosed))
	mustSubscribe(t, r, modulekit.Subscription{
		Ref: ref("t"), ModuleID: "abstains",
		Handler: func(context.Context, any) (any, error) { return nil, nil },
	})

	got, err := r.Transform(context.Background(), ref("t"), "original")
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	if got != "original" {
		t.Fatalf("got %v, want the value unchanged", got)
	}
}

// TestValidatorCollectsEveryRejection covers the ADR 0003 requirement that a
// caller sees all reasons, not just the first. Fixing one refusal only to
// discover the next is a poor experience for a user filling in a form.
func TestValidatorCollectsEveryRejection(t *testing.T) {
	r := hooks.New()
	mustOffer(t, r, "owner", point("check", modulekit.KindValidator, modulekit.FailClosed))

	for _, id := range []string{"credit", "fraud"} {
		reason := id + " says no"
		mustSubscribe(t, r, modulekit.Subscription{
			Ref: ref("check"), ModuleID: id,
			Handler: func(context.Context, any) (any, error) {
				return modulekit.Rejection{Reason: reason}, nil
			},
		})
	}
	mustSubscribe(t, r, modulekit.Subscription{
		Ref: ref("check"), ModuleID: "permissive",
		Handler: func(context.Context, any) (any, error) { return nil, nil },
	})

	err := r.Validate(context.Background(), ref("check"), "invoice")
	if !errors.Is(err, modulekit.ErrHookRejected) {
		t.Fatalf("expected ErrHookRejected, got %v", err)
	}

	rejections, ok := hooks.RejectionsFrom(err)
	if !ok {
		t.Fatal("expected structured rejections")
	}
	if len(rejections) != 2 {
		t.Fatalf("expected 2 rejections, got %d: %+v", len(rejections), rejections)
	}
	// Attribution is stamped by the dispatcher, not self-reported.
	for _, rej := range rejections {
		if rej.ModuleID == "" {
			t.Errorf("rejection not attributed to a module: %+v", rej)
		}
	}
}

// TestValidatorAcceptsWhenNoOneVetoes guards against the dispatcher treating
// "no subscribers objected" as a rejection.
func TestValidatorAcceptsWhenNoOneVetoes(t *testing.T) {
	r := hooks.New()
	mustOffer(t, r, "owner", point("check", modulekit.KindValidator, modulekit.FailClosed))
	mustSubscribe(t, r, modulekit.Subscription{
		Ref: ref("check"), ModuleID: "ok",
		Handler: func(context.Context, any) (any, error) { return nil, nil },
	})
	if err := r.Validate(context.Background(), ref("check"), "x"); err != nil {
		t.Fatalf("expected acceptance, got %v", err)
	}
}

// TestRejectionIsNotSubscriberFailure keeps the two conditions distinct: a
// business veto is an ordinary outcome; a broken subscriber is an incident.
// Reporting one as the other would make both unactionable.
func TestRejectionIsNotSubscriberFailure(t *testing.T) {
	r := hooks.New()
	mustOffer(t, r, "owner", point("check", modulekit.KindValidator, modulekit.FailClosed))
	mustSubscribe(t, r, modulekit.Subscription{
		Ref: ref("check"), ModuleID: "vetoes",
		Handler: func(context.Context, any) (any, error) {
			return modulekit.Rejection{Reason: "over limit"}, nil
		},
	})

	err := r.Validate(context.Background(), ref("check"), "x")
	if errors.Is(err, modulekit.ErrHookSubscriberFailed) {
		t.Fatalf("a veto was reported as a subscriber failure: %v", err)
	}
	if !errors.Is(err, modulekit.ErrHookRejected) {
		t.Fatalf("expected ErrHookRejected, got %v", err)
	}
}

// TestFailClosedAborts and TestFailOpenContinues are the two halves of ADR 0003
// decision 4. The policy must actually change behaviour, so both are asserted
// against the same failing subscriber.
func TestFailClosedAborts(t *testing.T) {
	r := hooks.New()
	mustOffer(t, r, "owner", point("obs", modulekit.KindObserver, modulekit.FailClosed))
	mustSubscribe(t, r, modulekit.Subscription{
		Ref: ref("obs"), ModuleID: "broken",
		Handler: func(context.Context, any) (any, error) { return nil, errors.New("boom") },
	})

	err := r.Notify(context.Background(), ref("obs"), "x")
	if !errors.Is(err, modulekit.ErrHookSubscriberFailed) {
		t.Fatalf("expected ErrHookSubscriberFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Errorf("error should name the failing subscriber, got: %v", err)
	}
}

func TestFailOpenContinues(t *testing.T) {
	r := hooks.New()
	mustOffer(t, r, "owner", point("obs", modulekit.KindObserver, modulekit.FailOpen))

	var reached bool
	mustSubscribe(t, r, modulekit.Subscription{
		Ref: ref("obs"), ModuleID: "broken", Priority: 0,
		Handler: func(context.Context, any) (any, error) { return nil, errors.New("boom") },
	})
	mustSubscribe(t, r, modulekit.Subscription{
		Ref: ref("obs"), ModuleID: "later", Priority: 1,
		Handler: func(context.Context, any) (any, error) { reached = true; return nil, nil },
	})

	if err := r.Notify(context.Background(), ref("obs"), "x"); err != nil {
		t.Fatalf("fail_open should not surface the failure: %v", err)
	}
	if !reached {
		t.Fatal("a failing fail_open subscriber stopped the dispatch")
	}
}

// TestFailOpenTransformerKeepsLastGoodValue: skipping a broken subscriber must
// not skip the value it was carrying.
func TestFailOpenTransformerKeepsLastGoodValue(t *testing.T) {
	r := hooks.New()
	mustOffer(t, r, "owner", point("t", modulekit.KindTransformer, modulekit.FailOpen))
	mustSubscribe(t, r, modulekit.Subscription{
		Ref: ref("t"), ModuleID: "good", Priority: 0,
		Handler: func(_ context.Context, v any) (any, error) { return v.(int) + 1, nil },
	})
	mustSubscribe(t, r, modulekit.Subscription{
		Ref: ref("t"), ModuleID: "broken", Priority: 1,
		Handler: func(context.Context, any) (any, error) { return 999, errors.New("boom") },
	})

	got, err := r.Transform(context.Background(), ref("t"), 1)
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	if got != 2 {
		t.Fatalf("got %v, want 2 — a failed subscriber's value leaked into the chain", got)
	}
}

// TestSubscriberTimeoutIsBounded: there is no unbounded wait on third-party
// code. The test would hang rather than fail if the timeout did not work.
func TestSubscriberTimeoutIsBounded(t *testing.T) {
	r := hooks.New()
	p := point("slow", modulekit.KindObserver, modulekit.FailClosed)
	p.Timeout = 50 * time.Millisecond
	mustOffer(t, r, "owner", p)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	mustSubscribe(t, r, modulekit.Subscription{
		Ref: ref("slow"), ModuleID: "hangs",
		Handler: func(context.Context, any) (any, error) { <-release; return nil, nil },
	})

	start := time.Now()
	err := r.Notify(context.Background(), ref("slow"), "x")
	elapsed := time.Since(start)

	if !errors.Is(err, modulekit.ErrHookSubscriberFailed) {
		t.Fatalf("expected a timeout failure, got %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("dispatch waited %s despite a 50ms timeout", elapsed)
	}
}

// TestSubscriberPanicDoesNotCrashHost: third-party code must not be able to
// take down the process. Without the recover this test panics the test binary.
func TestSubscriberPanicDoesNotCrashHost(t *testing.T) {
	r := hooks.New()
	mustOffer(t, r, "owner", point("obs", modulekit.KindObserver, modulekit.FailClosed))
	mustSubscribe(t, r, modulekit.Subscription{
		Ref: ref("obs"), ModuleID: "panics",
		Handler: func(context.Context, any) (any, error) { panic("third-party bug") },
	})

	err := r.Notify(context.Background(), ref("obs"), "x")
	if !errors.Is(err, modulekit.ErrHookSubscriberFailed) {
		t.Fatalf("expected the panic as an error, got %v", err)
	}
	if !strings.Contains(err.Error(), "panics") {
		t.Errorf("error should name the panicking subscriber, got: %v", err)
	}
}

// TestPanicOnFailOpenIsSkipped confirms the policy applies to panics too, not
// only to returned errors.
func TestPanicOnFailOpenIsSkipped(t *testing.T) {
	r := hooks.New()
	mustOffer(t, r, "owner", point("obs", modulekit.KindObserver, modulekit.FailOpen))
	mustSubscribe(t, r, modulekit.Subscription{
		Ref: ref("obs"), ModuleID: "panics",
		Handler: func(context.Context, any) (any, error) { panic("boom") },
	})
	if err := r.Notify(context.Background(), ref("obs"), "x"); err != nil {
		t.Fatalf("fail_open should absorb a panic, got %v", err)
	}
}

// TestKindMismatchIsRefused: the kind decides what happens to return values, so
// dispatching a validator through the observer path would silently discard
// every veto. That is a correctness-relevant confusion, not a style issue.
func TestKindMismatchIsRefused(t *testing.T) {
	r := hooks.New()
	mustOffer(t, r, "owner", point("check", modulekit.KindValidator, modulekit.FailClosed))
	mustSubscribe(t, r, modulekit.Subscription{
		Ref: ref("check"), ModuleID: "vetoes",
		Handler: func(context.Context, any) (any, error) {
			return modulekit.Rejection{Reason: "no"}, nil
		},
	})

	if err := r.Notify(context.Background(), ref("check"), "x"); !errors.Is(err, modulekit.ErrInvalidHookPoint) {
		t.Fatalf("validator dispatched as observer should be refused, got %v", err)
	}
	if _, err := r.Transform(context.Background(), ref("check"), "x"); !errors.Is(err, modulekit.ErrInvalidHookPoint) {
		t.Fatalf("validator dispatched as transformer should be refused, got %v", err)
	}
}

// TestDispatchingAnUnofferedHookFails: dispatching into nothing is a wiring
// error, and returning success would hide it.
func TestDispatchingAnUnofferedHookFails(t *testing.T) {
	r := hooks.New()
	if err := r.Notify(context.Background(), ref("ghost"), nil); !errors.Is(err, modulekit.ErrHookNotOffered) {
		t.Fatalf("expected ErrHookNotOffered, got %v", err)
	}
}

// TestHookWithNoSubscribersSucceeds is the opposite case: an offered hook that
// nobody uses is the normal state of most hook points.
func TestHookWithNoSubscribersSucceeds(t *testing.T) {
	r := hooks.New()
	mustOffer(t, r, "owner", point("lonely", modulekit.KindTransformer, modulekit.FailClosed))
	got, err := r.Transform(context.Background(), ref("lonely"), "value")
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	if got != "value" {
		t.Fatalf("got %v, want the value passed through untouched", got)
	}
}

// TestConcurrentDispatchIsSafe runs the race detector over parallel dispatch,
// since registration is single-threaded but dispatch happens on request
// goroutines.
func TestConcurrentDispatchIsSafe(t *testing.T) {
	r := hooks.New()
	mustOffer(t, r, "owner", point("t", modulekit.KindTransformer, modulekit.FailClosed))
	for i := 0; i < 4; i++ {
		mustSubscribe(t, r, modulekit.Subscription{
			Ref: ref("t"), ModuleID: fmt.Sprintf("mod-%d", i), Priority: i,
			Handler: func(_ context.Context, v any) (any, error) { return v.(int) + 1, nil },
		})
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := r.Transform(context.Background(), ref("t"), 0)
			if err != nil || got != 4 {
				t.Errorf("Transform = %v, %v; want 4, nil", got, err)
			}
		}()
	}
	wg.Wait()
}

// TestSubscriberIDsReportsDispatchOrder supports the debuggability the ADR
// calls for: answering "who participates in this flow" without running it.
func TestSubscriberIDsReportsDispatchOrder(t *testing.T) {
	r := hooks.New()
	mustOffer(t, r, "owner", point("t", modulekit.KindObserver, modulekit.FailOpen))
	mustSubscribe(t, r, modulekit.Subscription{Ref: ref("t"), ModuleID: "second", Priority: 5,
		Handler: func(context.Context, any) (any, error) { return nil, nil }})
	mustSubscribe(t, r, modulekit.Subscription{Ref: ref("t"), ModuleID: "first", Priority: 1,
		Handler: func(context.Context, any) (any, error) { return nil, nil }})

	got := r.SubscriberIDs(ref("t"))
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("got %v, want [first second]", got)
	}
}
