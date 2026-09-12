package wasmhost

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/getkayan/kayan/core/tenant"
	"github.com/sannados/sannad/pkg/modulekit"
)

// Internal-package tests: Module.hookHandler is unexported, and calling it
// directly is the narrowest way to test ADR 0007's hook-kind translation
// without also standing up a real hooks.Registry.

func guestV2BytesForHookTest(t *testing.T) []byte {
	t.Helper()
	wasm, err := os.ReadFile("testdata/guestv2.wasm")
	if err != nil {
		t.Fatalf("read guest v2: %v (rebuild with GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o testdata/guestv2.wasm ./testdata/guestv2/)", err)
	}
	return wasm
}

func newHookTestModule(t *testing.T, grant HookGrant) *Module {
	t.Helper()
	provided := modulekit.CapabilityRef{Name: "hook.guest", Version: "v1"}
	module, err := NewModule(context.Background(), ModuleConfig{
		Manifest: ApprovedManifest{
			ID:              "com.example.hookguest",
			ABIVersion:      CurrentABIVersion,
			Provides:        []modulekit.CapabilityRef{provided},
			SubscribesHooks: []HookGrant{grant},
		},
		Wasm:    guestV2BytesForHookTest(t),
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("new module: %v", err)
	}
	t.Cleanup(func() { _ = module.Close(context.Background()) })
	return module
}

func tenantCtxForHookTest(id string) context.Context {
	return tenant.WithTenantID(context.Background(), id)
}

// TestValidatorHookRejectionBecomesARejection: a v2 guest subscribed as a
// validator can veto, and its veto arrives as modulekit.Rejection rather than
// an error — the same contract an in-process validator subscriber has.
func TestValidatorHookRejectionBecomesARejection(t *testing.T) {
	grant := HookGrant{Ref: modulekit.HookRef{Name: "orders.before_submit", Version: "v1"}, Kind: modulekit.KindValidator}
	module := newHookTestModule(t, grant)

	handler := module.hookHandler(grant)
	result, err := handler(tenantCtxForHookTest("acme"), modulekit.RawPayload(`{"reject":true}`))
	if err != nil {
		t.Fatalf("a validator veto was reported as an error: %v", err)
	}
	rejection, ok := result.(modulekit.Rejection)
	if !ok {
		t.Fatalf("got %T, want modulekit.Rejection", result)
	}
	if rejection.Reason != "rejected by guest" {
		t.Fatalf("reason = %q, want %q", rejection.Reason, "rejected by guest")
	}
}

// TestValidatorHookAcceptanceReturnsNil: the acceptance path of the same
// contract.
func TestValidatorHookAcceptanceReturnsNil(t *testing.T) {
	grant := HookGrant{Ref: modulekit.HookRef{Name: "orders.before_submit", Version: "v1"}, Kind: modulekit.KindValidator}
	module := newHookTestModule(t, grant)

	handler := module.hookHandler(grant)
	result, err := handler(tenantCtxForHookTest("acme"), modulekit.RawPayload(`{"reject":false}`))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if result != nil {
		t.Fatalf("got %v, want nil for acceptance", result)
	}
}

// TestTransformerHookThreadsGuestOutput: a transformer's return value is what
// the caller receives back, decoded as RawPayload the same way a capability
// response is.
func TestTransformerHookThreadsGuestOutput(t *testing.T) {
	grant := HookGrant{Ref: modulekit.HookRef{Name: "pricing.adjust", Version: "v1"}, Kind: modulekit.KindTransformer}
	module := newHookTestModule(t, grant)

	handler := module.hookHandler(grant)
	result, err := handler(tenantCtxForHookTest("acme"), modulekit.RawPayload(`{"amount":5}`))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	raw, ok := result.(modulekit.RawPayload)
	if !ok {
		t.Fatalf("got %T, want modulekit.RawPayload", result)
	}
	var out struct {
		Amount int `json:"amount"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Amount != 10 {
		t.Fatalf("amount = %d, want 10", out.Amount)
	}
}

// TestObserverHookIgnoresGuestReturnValue: an observer's return is discarded
// by contract regardless of what the guest sent back.
func TestObserverHookIgnoresGuestReturnValue(t *testing.T) {
	grant := HookGrant{Ref: modulekit.HookRef{Name: "orders.after_submit", Version: "v1"}, Kind: modulekit.KindObserver}
	module := newHookTestModule(t, grant)

	handler := module.hookHandler(grant)
	result, err := handler(tenantCtxForHookTest("acme"), modulekit.RawPayload(`{"reject":true}`))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if result != nil {
		t.Fatalf("got %v, want nil: an observer's return value is always ignored", result)
	}
}

// TestNoTenantIsRefusedForAHook: the same rule v1 capability invocation
// enforces applies to inbound hook dispatch — a guest must not run for no
// identifiable tenant.
func TestNoTenantIsRefusedForAHook(t *testing.T) {
	grant := HookGrant{Ref: modulekit.HookRef{Name: "orders.before_submit", Version: "v1"}, Kind: modulekit.KindValidator}
	module := newHookTestModule(t, grant)

	handler := module.hookHandler(grant)
	if _, err := handler(context.Background(), modulekit.RawPayload(`{}`)); err == nil {
		t.Fatal("a hook ran with no tenant in context")
	}
}

// TestHookKindMismatchIsRefusedAtInstall: the manifest's expected kind must
// match how the point is actually offered — checked at installation, not
// discovered the first time the hook fires.
func TestHookKindMismatchIsRefusedAtInstall(t *testing.T) {
	grant := HookGrant{Ref: modulekit.HookRef{Name: "orders.before_submit", Version: "v1"}, Kind: modulekit.KindValidator}
	module := newHookTestModule(t, grant)

	reg := &kindAssertingRegistrar{
		offered: []modulekit.HookPoint{{
			Ref:     grant.Ref,
			Kind:    modulekit.KindTransformer, // offered differently than the manifest expects
			Policy:  modulekit.FailClosed,
			Timeout: time.Second,
		}},
	}
	if err := module.Install(reg); err == nil {
		t.Fatal("a hook kind mismatch between the manifest and the offered point was accepted")
	}
}

// kindAssertingRegistrar implements enough of modulekit.Registrar plus the
// offeredPointsLister surface for Install's kind check to exercise.
type kindAssertingRegistrar struct {
	offered []modulekit.HookPoint
}

func (r *kindAssertingRegistrar) RegisterCapability(modulekit.CapabilityRef, modulekit.Handler) error {
	return nil
}

func (r *kindAssertingRegistrar) OfferHook(modulekit.HookPoint) error { return nil }

func (r *kindAssertingRegistrar) Subscribe(modulekit.Subscription) error { return nil }

func (r *kindAssertingRegistrar) OfferedPoints() []modulekit.HookPoint { return r.offered }

var _ modulekit.HookRegistrar = (*kindAssertingRegistrar)(nil)
var _ offeredPointsLister = (*kindAssertingRegistrar)(nil)
