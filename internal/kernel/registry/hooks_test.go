package registry_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sannados/sannad/internal/kernel/registry"
	"github.com/sannados/sannad/pkg/modulekit"
)

var pointRef = modulekit.HookRef{Name: "flow.before_commit", Version: "v1"}

// hookModule is a configurable test module that offers a hook point and/or
// subscribes to one, so registration wiring can be exercised directly.
type hookModule struct {
	id        string
	offers    []modulekit.HookPoint
	subscribe *modulekit.Subscription
	installFn func(modulekit.Registrar) error
}

func (m *hookModule) Descriptor() modulekit.Descriptor {
	return modulekit.Descriptor{ID: m.id, Kind: "embedded", Storage: modulekit.StorageNone, Offers: m.offers}
}

func (m *hookModule) Install(reg modulekit.Registrar) error {
	if m.installFn != nil {
		return m.installFn(reg)
	}
	if m.subscribe != nil {
		hr, ok := modulekit.HookRegistrarFrom(reg)
		if !ok {
			return errors.New("registrar has no hook surface")
		}
		return hr.Subscribe(*m.subscribe)
	}
	return nil
}

func (m *hookModule) Start(context.Context) error { return nil }
func (m *hookModule) Stop(context.Context) error  { return nil }

func validatorPoint() modulekit.HookPoint {
	return modulekit.HookPoint{
		Ref:     pointRef,
		Kind:    modulekit.KindValidator,
		Policy:  modulekit.FailClosed,
		Timeout: time.Second,
	}
}

// TestDescriptorHookPointsAreOffered confirms a module declaring hook points in
// its descriptor does not also have to register them imperatively.
func TestDescriptorHookPointsAreOffered(t *testing.T) {
	reg := registry.New()
	if err := reg.RegisterModule(&hookModule{id: "owner", offers: []modulekit.HookPoint{validatorPoint()}}); err != nil {
		t.Fatalf("RegisterModule: %v", err)
	}

	points := reg.Hooks().OfferedPoints()
	if len(points) != 1 || points[0].Ref.Key() != pointRef.Key() {
		t.Fatalf("expected the descriptor hook point to be offered, got %+v", points)
	}
}

// TestSubscriberIdentityComesFromTheHost is the attribution guarantee: a module
// cannot subscribe under another module's name, so the module named in a
// rejection or a failure log is the module that actually ran.
func TestSubscriberIdentityComesFromTheHost(t *testing.T) {
	reg := registry.New()
	if err := reg.RegisterModule(&hookModule{id: "owner", offers: []modulekit.HookPoint{validatorPoint()}}); err != nil {
		t.Fatalf("register owner: %v", err)
	}

	liar := &hookModule{
		id: "honest-id",
		subscribe: &modulekit.Subscription{
			Ref:      pointRef,
			ModuleID: "some-other-module", // claims to be someone else
			Handler:  func(context.Context, any) (any, error) { return nil, nil },
		},
	}
	if err := reg.RegisterModule(liar); err != nil {
		t.Fatalf("register subscriber: %v", err)
	}

	ids := reg.Hooks().SubscriberIDs(pointRef)
	if len(ids) != 1 {
		t.Fatalf("expected 1 subscriber, got %v", ids)
	}
	if ids[0] != "honest-id" {
		t.Fatalf("subscriber registered as %q; the claimed ID overrode the real one", ids[0])
	}
}

// TestSubscribingOutsideInstallIsRefused: without an installing module there is
// no trustworthy identity to attribute a subscription to, so the registry
// refuses rather than inventing one.
func TestSubscribingOutsideInstallIsRefused(t *testing.T) {
	reg := registry.New()
	if err := reg.RegisterModule(&hookModule{id: "owner", offers: []modulekit.HookPoint{validatorPoint()}}); err != nil {
		t.Fatalf("register owner: %v", err)
	}

	err := reg.Subscribe(modulekit.Subscription{
		Ref:      pointRef,
		ModuleID: "drive-by",
		Handler:  func(context.Context, any) (any, error) { return nil, nil },
	})
	if !errors.Is(err, modulekit.ErrInvalidHookPoint) {
		t.Fatalf("expected refusal outside install, got %v", err)
	}
}

// TestFailedInstallDoesNotLeaveTheModuleRegistered covers rollback: hook points
// are offered before Install, so a module whose Install fails must not leave a
// half-registered entry behind.
func TestFailedInstallDoesNotLeaveTheModuleRegistered(t *testing.T) {
	reg := registry.New()
	boom := errors.New("install failed")
	m := &hookModule{
		id:        "broken",
		offers:    []modulekit.HookPoint{validatorPoint()},
		installFn: func(modulekit.Registrar) error { return boom },
	}
	if err := reg.RegisterModule(m); !errors.Is(err, boom) {
		t.Fatalf("expected the install error, got %v", err)
	}

	// Re-registering must not report a duplicate, which would mean the failed
	// module was still recorded.
	if err := reg.RegisterModule(&hookModule{id: "broken"}); err != nil {
		t.Fatalf("re-registering after a failed install: %v", err)
	}
}

// TestSubscribeToUnofferedHookFailsInstall makes the ordering consequence
// explicit: subscribing before the owner is registered is a wiring error that
// surfaces at startup, not a subscription that silently never fires.
func TestSubscribeToUnofferedHookFailsInstall(t *testing.T) {
	reg := registry.New()
	m := &hookModule{
		id: "early",
		subscribe: &modulekit.Subscription{
			Ref:     pointRef,
			Handler: func(context.Context, any) (any, error) { return nil, nil },
		},
	}
	if err := reg.RegisterModule(m); !errors.Is(err, modulekit.ErrHookNotOffered) {
		t.Fatalf("expected ErrHookNotOffered, got %v", err)
	}
}

// TestModuleCanSubscribeToItsOwnHookPoint: points are offered before Install
// runs, so a module can participate in the flow it owns.
func TestModuleCanSubscribeToItsOwnHookPoint(t *testing.T) {
	reg := registry.New()
	m := &hookModule{
		id:     "self",
		offers: []modulekit.HookPoint{validatorPoint()},
		subscribe: &modulekit.Subscription{
			Ref:     pointRef,
			Handler: func(context.Context, any) (any, error) { return nil, nil },
		},
	}
	if err := reg.RegisterModule(m); err != nil {
		t.Fatalf("RegisterModule: %v", err)
	}
	if ids := reg.Hooks().SubscriberIDs(pointRef); len(ids) != 1 || ids[0] != "self" {
		t.Fatalf("got %v, want [self]", ids)
	}
}

// TestMalformedDescriptorHookPointFailsRegistration keeps the declaration
// validated wherever it enters the system, not only through OfferHook.
func TestMalformedDescriptorHookPointFailsRegistration(t *testing.T) {
	reg := registry.New()
	bad := validatorPoint()
	bad.Timeout = 0

	err := reg.RegisterModule(&hookModule{id: "bad", offers: []modulekit.HookPoint{bad}})
	if !errors.Is(err, modulekit.ErrInvalidHookPoint) {
		t.Fatalf("expected ErrInvalidHookPoint, got %v", err)
	}
}

// TestEndToEndVeto exercises the whole path: one module offers a flow, another
// vetoes it, and the owner sees an attributed rejection. This is the ecosystem
// property the hook system exists for — extension without modification.
func TestEndToEndVeto(t *testing.T) {
	reg := registry.New()
	if err := reg.RegisterModule(&hookModule{id: "flow-owner", offers: []modulekit.HookPoint{validatorPoint()}}); err != nil {
		t.Fatalf("register owner: %v", err)
	}

	extension := &hookModule{
		id: "credit-check",
		subscribe: &modulekit.Subscription{
			Ref: pointRef,
			Handler: func(_ context.Context, value any) (any, error) {
				if value.(int) > 100 {
					return modulekit.Rejection{Reason: "over credit limit"}, nil
				}
				return nil, nil
			},
		},
	}
	if err := reg.RegisterModule(extension); err != nil {
		t.Fatalf("register extension: %v", err)
	}

	if err := reg.Hooks().Validate(context.Background(), pointRef, 50); err != nil {
		t.Fatalf("under the limit should pass: %v", err)
	}

	err := reg.Hooks().Validate(context.Background(), pointRef, 500)
	if !errors.Is(err, modulekit.ErrHookRejected) {
		t.Fatalf("expected a veto, got %v", err)
	}
	if !errors.Is(err, modulekit.ErrHookRejected) || err.Error() == "" {
		t.Fatalf("veto carried no reason: %v", err)
	}
}
