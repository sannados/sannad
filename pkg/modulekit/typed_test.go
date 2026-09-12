package modulekit_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sannados/sannad/pkg/modulekit"
)

type getReq struct{ ID string }
type getResp struct{ Name string }

// capturingRegistrar records what HandleTyped registered so the wrapper can be
// invoked the way the bus would invoke it.
type capturingRegistrar struct {
	handlers map[string]modulekit.Handler
}

func (c *capturingRegistrar) RegisterCapability(ref modulekit.CapabilityRef, h modulekit.Handler) error {
	if c.handlers == nil {
		c.handlers = map[string]modulekit.Handler{}
	}
	c.handlers[ref.Key()] = h
	return nil
}

var testRef = modulekit.CapabilityRef{Name: "test.get", Version: "v1"}

func TestHandleTypedRemovesTheAssertion(t *testing.T) {
	reg := &capturingRegistrar{}
	err := modulekit.HandleTyped(reg, testRef, func(_ context.Context, req getReq) (getResp, error) {
		return getResp{Name: "contact-" + req.ID}, nil
	})
	if err != nil {
		t.Fatalf("HandleTyped: %v", err)
	}

	result, err := reg.handlers[testRef.Key()](context.Background(), getReq{ID: "42"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if result.(getResp).Name != "contact-42" {
		t.Fatalf("got %+v", result)
	}
}

// TestHandleTypedAcceptsPointerForm: passing &req to a handler declared over
// req is a natural thing for a caller to write, and failing on it would be a
// pointless trap.
func TestHandleTypedAcceptsPointerForm(t *testing.T) {
	reg := &capturingRegistrar{}
	_ = modulekit.HandleTyped(reg, testRef, func(_ context.Context, req getReq) (getResp, error) {
		return getResp{Name: req.ID}, nil
	})

	result, err := reg.handlers[testRef.Key()](context.Background(), &getReq{ID: "ptr"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if result.(getResp).Name != "ptr" {
		t.Fatalf("got %+v", result)
	}
}

// TestTypeMismatchIsReported: two modules disagreeing about a contract is a
// wiring error. It must surface as one rather than as a nil result or a panic,
// which is the failure mode the untyped Handler had.
func TestTypeMismatchIsReported(t *testing.T) {
	reg := &capturingRegistrar{}
	_ = modulekit.HandleTyped(reg, testRef, func(_ context.Context, req getReq) (getResp, error) {
		return getResp{}, nil
	})

	_, err := reg.handlers[testRef.Key()](context.Background(), "not a getReq")
	if !errors.Is(err, modulekit.ErrTypeMismatch) {
		t.Fatalf("expected ErrTypeMismatch, got %v", err)
	}
	// The error names the capability, so a mismatch in a large wiring graph is
	// locatable without a stack trace.
	if !strings.Contains(err.Error(), testRef.Key()) {
		t.Errorf("error should name the capability, got: %v", err)
	}
}

func TestHandlerErrorPropagates(t *testing.T) {
	reg := &capturingRegistrar{}
	sentinel := errors.New("handler failed")
	_ = modulekit.HandleTyped(reg, testRef, func(_ context.Context, req getReq) (getResp, error) {
		return getResp{}, sentinel
	})

	_, err := reg.handlers[testRef.Key()](context.Background(), getReq{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the sentinel back, got %v", err)
	}
}

// hookRecorder implements modulekit.HookRegistrar for the typed-subscriber
// tests.
type hookRecorder struct{ subs []modulekit.Subscription }

func (h *hookRecorder) OfferHook(modulekit.HookPoint) error { return nil }
func (h *hookRecorder) Subscribe(sub modulekit.Subscription) error {
	h.subs = append(h.subs, sub)
	return nil
}

var hookRef = modulekit.HookRef{Name: "test.hook", Version: "v1"}

func TestSubscribeTypedPassesTheValue(t *testing.T) {
	rec := &hookRecorder{}
	err := modulekit.SubscribeTyped(rec, hookRef, "mod", 5, func(_ context.Context, v getReq) (any, error) {
		return modulekit.Rejection{Reason: "saw " + v.ID}, nil
	})
	if err != nil {
		t.Fatalf("SubscribeTyped: %v", err)
	}
	if len(rec.subs) != 1 || rec.subs[0].Priority != 5 || rec.subs[0].ModuleID != "mod" {
		t.Fatalf("subscription not recorded correctly: %+v", rec.subs)
	}

	result, err := rec.subs[0].Handler(context.Background(), getReq{ID: "x"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if result.(modulekit.Rejection).Reason != "saw x" {
		t.Fatalf("got %+v", result)
	}
}

// TestTransformTypedKeepsTheType is the reason TransformTyped exists: a
// transformer returning any would put the type assertion back in the caller.
func TestTransformTypedKeepsTheType(t *testing.T) {
	rec := &hookRecorder{}
	err := modulekit.TransformTyped(rec, hookRef, "mod", 0, func(_ context.Context, n int) (int, error) {
		return n * 3, nil
	})
	if err != nil {
		t.Fatalf("TransformTyped: %v", err)
	}

	result, err := rec.subs[0].Handler(context.Background(), 7)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if result != 21 {
		t.Fatalf("got %v, want 21", result)
	}
}

func TestSubscribeTypedReportsMismatch(t *testing.T) {
	rec := &hookRecorder{}
	_ = modulekit.SubscribeTyped(rec, hookRef, "mod", 0, func(_ context.Context, v getReq) (any, error) {
		return nil, nil
	})

	_, err := rec.subs[0].Handler(context.Background(), 12345)
	if !errors.Is(err, modulekit.ErrTypeMismatch) {
		t.Fatalf("expected ErrTypeMismatch, got %v", err)
	}
	// Both the hook and the subscriber are named, since either side could be
	// the one that is wrong.
	if !strings.Contains(err.Error(), hookRef.Key()) || !strings.Contains(err.Error(), "mod") {
		t.Errorf("error should name hook and subscriber, got: %v", err)
	}
}

// TestHookPointValidation covers the SDK-side validation, mirroring what the
// registry enforces, so a malformed declaration is caught even by a host that
// forgets to validate.
func TestHookPointValidation(t *testing.T) {
	good := modulekit.HookPoint{
		Ref:     hookRef,
		Kind:    modulekit.KindValidator,
		Policy:  modulekit.FailClosed,
		Timeout: time.Second,
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid hook point rejected: %v", err)
	}

	bad := good
	bad.Policy = ""
	if err := bad.Validate(); !errors.Is(err, modulekit.ErrInvalidHookPoint) {
		t.Fatalf("expected ErrInvalidHookPoint, got %v", err)
	}
}

// stubCaller stands in for the kernel bus. That a one-struct double suffices is
// the point of Caller being an interface: the SDK names no kernel package.
type stubCaller struct {
	response any
	err      error
	gotReq   any
}

func (s *stubCaller) Call(_ context.Context, _ modulekit.CapabilityRef, request any) (any, error) {
	s.gotReq = request
	return s.response, s.err
}

func TestCallTypedReturnsTheConcreteType(t *testing.T) {
	caller := &stubCaller{response: getResp{Name: "hi"}}

	resp, err := modulekit.CallTyped[getReq, getResp](
		context.Background(), caller, testRef, getReq{ID: "amina"})
	if err != nil {
		t.Fatalf("CallTyped: %v", err)
	}
	if resp.Name != "hi" {
		t.Fatalf("got %q, want %q", resp.Name, "hi")
	}
	// The request must arrive as the typed value, not re-encoded. In-process
	// calls pay no serialization, which is what makes the embedded tier fast.
	if _, ok := caller.gotReq.(getReq); !ok {
		t.Fatalf("request reached the caller as %T, want getReq", caller.gotReq)
	}
}

// TestCallTypedReportsAResponseMismatch: two modules disagreeing about a shared
// contract is a wiring error, and it must name the capability — an ErrTypeMismatch
// with no capability in it is unactionable.
func TestCallTypedReportsAResponseMismatch(t *testing.T) {
	caller := &stubCaller{response: "not a getResp"}

	_, err := modulekit.CallTyped[getReq, getResp](
		context.Background(), caller, testRef, getReq{})
	if !errors.Is(err, modulekit.ErrTypeMismatch) {
		t.Fatalf("expected ErrTypeMismatch, got %v", err)
	}
	if !strings.Contains(err.Error(), testRef.Key()) {
		t.Errorf("error does not name the capability: %v", err)
	}
}

// TestCallTypedPropagatesTheCallError: a provider's failure must reach the
// caller unwrapped enough to test with errors.Is.
func TestCallTypedPropagatesTheCallError(t *testing.T) {
	boom := errors.New("provider exploded")
	caller := &stubCaller{err: boom}

	_, err := modulekit.CallTyped[getReq, getResp](
		context.Background(), caller, testRef, getReq{})
	if !errors.Is(err, boom) {
		t.Fatalf("expected the provider error, got %v", err)
	}
}
