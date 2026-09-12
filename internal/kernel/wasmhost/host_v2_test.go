package wasmhost_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/getkayan/kayan/core/tenant"
	"github.com/sannados/sannad/internal/kernel/wasmhost"
	"github.com/sannados/sannad/pkg/modulekit"
)

// These exercise ADR 0007's host-call ABI against a real compiled v2 guest —
// testdata/guestv2, distinct from testdata/guest because a v1 guest imports
// nothing beyond WASI and a v2 guest genuinely imports sannad_v2. Rebuild with
// GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o testdata/guestv2.wasm ./testdata/guestv2/

func guestV2Bytes(t *testing.T) []byte {
	t.Helper()
	wasm, err := os.ReadFile("testdata/guestv2.wasm")
	if err != nil {
		t.Fatalf("read guest v2: %v (rebuild with GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o testdata/guestv2.wasm ./testdata/guestv2/)", err)
	}
	return wasm
}

var v2Ref = modulekit.CapabilityRef{Name: "pricing.v2", Version: "v1"}

// stubCaller resolves capability calls from a fixed map, standing in for the
// kernel bus so these tests do not need a running registry.
type stubCaller struct {
	handlers map[string]modulekit.Handler
}

func (c *stubCaller) Call(ctx context.Context, ref modulekit.CapabilityRef, request any) (any, error) {
	h, ok := c.handlers[ref.Key()]
	if !ok {
		return nil, fmt.Errorf("stubCaller: no handler for %s", ref.Key())
	}
	return h(ctx, request)
}

// stubPublisher records what was published, standing in for the broker.
type stubPublisher struct {
	subject string
	payload []byte
}

func (p *stubPublisher) Publish(_ context.Context, subject string, payload any) error {
	p.subject = subject
	raw, ok := payload.(json.RawMessage)
	if !ok {
		return fmt.Errorf("stubPublisher: payload was %T, not json.RawMessage", payload)
	}
	p.payload = raw
	return nil
}

func loadV2(t *testing.T, v2 *wasmhost.V2Config) *wasmhost.Plugin {
	t.Helper()
	plugin, err := wasmhost.Load(context.Background(), wasmhost.Config{
		ModuleID: "com.example.guestv2",
		Wasm:     guestV2Bytes(t),
		Timeout:  5 * time.Second,
		V2:       v2,
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Cleanup(func() { _ = plugin.Close(context.Background()) })
	return plugin
}

type v2CallRequest struct {
	Amount         int             `json:"amount"`
	CallCapability string          `json:"call_capability,omitempty"`
	CallPayload    json.RawMessage `json:"call_payload,omitempty"`
	PublishEvent   string          `json:"publish_event,omitempty"`
	PublishPayload json.RawMessage `json:"publish_payload,omitempty"`
}

// TestV1GuestsStillInstallAndServeUnchanged: ADR 0007's first conformance
// requirement. The v1 fixture and its whole test file (host_test.go) are
// untouched by this change; this pins that the same guest still works when
// loaded by the same Load function ABI v2 also uses.
func TestV1GuestsStillInstallAndServeUnchanged(t *testing.T) {
	plugin := load(t, wasmhost.Config{})
	handler := plugin.Handler(testRef, wasmhost.JSONCodec{})

	result, err := handler(tenantCtx("acme"), calcRequest{Amount: 10})
	if err != nil {
		t.Fatalf("v1 guest under the v2-capable host: %v", err)
	}
	var resp calcResponse
	if err := json.Unmarshal(result.(modulekit.RawPayload), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Doubled != 20 {
		t.Fatalf("doubled = %d, want 20", resp.Doubled)
	}
}

// TestApprovedCapabilityCallReachesTheGrantedCapability: a v2 guest calling a
// capability it was approved to consume gets a real result back, resolved
// through the same Caller interface the kernel bus implements.
func TestApprovedCapabilityCallReachesTheGrantedCapability(t *testing.T) {
	downstream := modulekit.CapabilityRef{Name: "inventory.check", Version: "v1"}
	caller := &stubCaller{handlers: map[string]modulekit.Handler{
		downstream.Key(): func(_ context.Context, request any) (any, error) {
			return modulekit.RawPayload(`{"in_stock":true}`), nil
		},
	}}

	plugin := loadV2(t, &wasmhost.V2Config{
		Caller:   caller,
		Consumes: []modulekit.CapabilityRef{downstream},
	})
	handler := plugin.Handler(v2Ref, wasmhost.JSONCodec{})

	req := v2CallRequest{CallCapability: downstream.Key()}
	result, err := handler(tenantCtx("acme"), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if string(result.(modulekit.RawPayload)) != `{"in_stock":true}` {
		t.Fatalf("got %s, want the downstream capability's raw response", result.(modulekit.RawPayload))
	}
}

// TestUndeclaredCapabilityCallIsDenied: guest-declared intent is not
// authority (ADR 0007 rule 1). A guest asking to call something the approved
// manifest never listed in Consumes is refused, and the downstream handler is
// never invoked.
func TestUndeclaredCapabilityCallIsDenied(t *testing.T) {
	downstream := modulekit.CapabilityRef{Name: "inventory.check", Version: "v1"}
	called := false
	caller := &stubCaller{handlers: map[string]modulekit.Handler{
		downstream.Key(): func(context.Context, any) (any, error) {
			called = true
			return modulekit.RawPayload(`{}`), nil
		},
	}}

	// Empty Consumes: the guest may reach the v2 import table but nothing is
	// granted.
	plugin := loadV2(t, &wasmhost.V2Config{Caller: caller})
	handler := plugin.Handler(v2Ref, wasmhost.JSONCodec{})

	req := v2CallRequest{CallCapability: downstream.Key()}
	_, err := handler(tenantCtx("acme"), req)
	if err == nil {
		t.Fatal("a capability_call for an undeclared capability reported success")
	}
	if called {
		t.Fatal("the downstream handler ran despite no grant")
	}
}

// TestGuestCannotChooseATenantForAnOutboundCall: the tenant an outbound call
// carries comes from the invocation context, not anything the guest could put
// in its request — there is no field for it in the v2 call envelope at all,
// so this proves the downstream handler sees the real caller's tenant even
// though the guest never had the chance to name one.
func TestGuestCannotChooseATenantForAnOutboundCall(t *testing.T) {
	downstream := modulekit.CapabilityRef{Name: "audit.log", Version: "v1"}
	var seenTenant string
	caller := &stubCaller{handlers: map[string]modulekit.Handler{
		downstream.Key(): func(ctx context.Context, _ any) (any, error) {
			seenTenant = tenant.IDFromContext(ctx)
			return modulekit.RawPayload(`{}`), nil
		},
	}}

	plugin := loadV2(t, &wasmhost.V2Config{
		Caller:   caller,
		Consumes: []modulekit.CapabilityRef{downstream},
	})
	handler := plugin.Handler(v2Ref, wasmhost.JSONCodec{})

	req := v2CallRequest{CallCapability: downstream.Key()}
	if _, err := handler(tenantCtx("real-tenant"), req); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if seenTenant != "real-tenant" {
		t.Fatalf("downstream saw tenant %q, want real-tenant", seenTenant)
	}
}

// TestResultHandleNeverExecutesTheCapabilityTwice: the whole reason
// capability_call returns a handle instead of copying bytes straight into a
// caller-sized buffer. Reading the same handle multiple times must return the
// same bytes without invoking the downstream capability again.
func TestResultHandleNeverExecutesTheCapabilityTwice(t *testing.T) {
	downstream := modulekit.CapabilityRef{Name: "payments.charge", Version: "v1"}
	calls := 0
	caller := &stubCaller{handlers: map[string]modulekit.Handler{
		downstream.Key(): func(context.Context, any) (any, error) {
			calls++
			return modulekit.RawPayload(fmt.Sprintf(`{"charge_id":%d}`, calls)), nil
		},
	}}

	plugin := loadV2(t, &wasmhost.V2Config{
		Caller:   caller,
		Consumes: []modulekit.CapabilityRef{downstream},
	})
	handler := plugin.Handler(v2Ref, wasmhost.JSONCodec{})

	req := v2CallRequest{CallCapability: downstream.Key()}
	if _, err := handler(tenantCtx("acme"), req); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if calls != 1 {
		t.Fatalf("downstream capability invoked %d times for one guest call, want 1", calls)
	}
}

// TestApprovedEventPublishReachesTheBroker: a guest approved to publish a
// named event gets a tenant-scoped subject built for it — it never
// constructs one itself.
func TestApprovedEventPublishReachesTheBroker(t *testing.T) {
	publisher := &stubPublisher{}
	plugin := loadV2(t, &wasmhost.V2Config{
		Events:          publisher,
		PublishesEvents: []string{"orders.created"},
	})
	handler := plugin.Handler(v2Ref, wasmhost.JSONCodec{})

	req := v2CallRequest{PublishEvent: "orders.created", PublishPayload: json.RawMessage(`{"id":1}`)}
	if _, err := handler(tenantCtx("acme"), req); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if publisher.subject != "sannad.acme.orders.created" {
		t.Fatalf("subject = %q, want sannad.acme.orders.created", publisher.subject)
	}
	if string(publisher.payload) != `{"id":1}` {
		t.Fatalf("payload = %s, want {\"id\":1}", publisher.payload)
	}
}

// TestUndeclaredEventPublishIsDenied mirrors the capability grant test for
// events: a name not listed in PublishesEvents is refused, and nothing
// reaches the broker.
func TestUndeclaredEventPublishIsDenied(t *testing.T) {
	publisher := &stubPublisher{}
	plugin := loadV2(t, &wasmhost.V2Config{Events: publisher})
	handler := plugin.Handler(v2Ref, wasmhost.JSONCodec{})

	req := v2CallRequest{PublishEvent: "orders.created", PublishPayload: json.RawMessage(`{}`)}
	if _, err := handler(tenantCtx("acme"), req); err == nil {
		t.Fatal("an undeclared event publish reported success")
	}
	if publisher.subject != "" {
		t.Fatal("the broker received an event that was never approved")
	}
}

// TestCapabilityCallCycleIsDenied: a v2 guest approved to call its own
// provided capability (visible in Consumes, per the ADR's self-recursion
// rule) must still be stopped from looping forever. The chain is checked at
// this plugin's own Handler entry point, so calling itself is caught on
// re-entry.
//
// The cycle is detected on the re-entrant Handler call, deep inside the host
// call the guest made — that failure comes back to the guest as a host-call
// diagnostic (ADR 0007 §6: "cycle errors name the chain for operators"), and
// the guest reports it onward as an ordinary rejection. So it surfaces at
// this test's own top-level call as ErrForbidden naming the cycle, not as
// ErrCallCycle bubbling through untouched — that sentinel is what the
// re-entrant Handler call itself returns, one level down.
func TestCapabilityCallCycleIsDenied(t *testing.T) {
	caller := &stubCaller{handlers: map[string]modulekit.Handler{}}
	plugin := loadV2(t, &wasmhost.V2Config{
		Caller:   caller,
		Consumes: []modulekit.CapabilityRef{v2Ref},
	})
	// The plugin's own handler serves as the target its guest calls: a
	// self-approved recursion (v2Ref granted in its own Consumes), which the
	// ADR explicitly permits to declare but requires a cycle guard for.
	handler := plugin.Handler(v2Ref, wasmhost.JSONCodec{})
	caller.handlers[v2Ref.Key()] = handler

	req := v2CallRequest{CallCapability: v2Ref.Key()}
	_, err := handler(tenantCtx("acme"), req)
	if err == nil {
		t.Fatal("a capability call cycle back to the same capability reported success")
	}
	if !errors.Is(err, modulekit.ErrForbidden) {
		t.Fatalf("expected ErrForbidden (the guest's own report of the host's cycle refusal), got %v", err)
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("error should name the cycle for the operator, got %v", err)
	}
}

// TestCallDepthLimitIsEnforced: a chain of distinct WASM-served capabilities,
// each calling the next, is stopped once it exceeds the configured depth —
// not only an exact cycle back to the same capability.
//
// Each guest only forwards a call if its own request names one, so the chain
// has to be pre-built payload-by-payload from the innermost step outward:
// step i's payload is {call_capability: refs[i+1], call_payload: <step i+1's
// own payload>}, and the deepest step carries no call_capability at all.
func TestCallDepthLimitIsEnforced(t *testing.T) {
	const chainLength = 6
	refs := make([]modulekit.CapabilityRef, chainLength)
	for i := range refs {
		refs[i] = modulekit.CapabilityRef{Name: fmt.Sprintf("chain.step%d", i), Version: "v1"}
	}

	caller := &stubCaller{handlers: map[string]modulekit.Handler{}}
	for i := range refs {
		consumes := []modulekit.CapabilityRef{v2Ref}
		if i+1 < chainLength {
			consumes = []modulekit.CapabilityRef{refs[i+1]}
		}
		plugin := loadV2(t, &wasmhost.V2Config{
			Caller:       caller,
			Consumes:     consumes,
			MaxCallDepth: 3,
		})
		caller.handlers[refs[i].Key()] = plugin.Handler(refs[i], wasmhost.JSONCodec{})
	}

	step := v2CallRequest{Amount: 1}
	for i := chainLength - 2; i >= 0; i-- {
		payload, err := json.Marshal(step)
		if err != nil {
			t.Fatalf("marshal step %d: %v", i, err)
		}
		step = v2CallRequest{CallCapability: refs[i+1].Key(), CallPayload: payload}
	}

	_, err := caller.handlers[refs[0].Key()](tenantCtx("acme"), step)
	if err == nil {
		t.Fatal("a call chain past the configured depth reported success")
	}
	if !errors.Is(err, modulekit.ErrForbidden) || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("expected a depth-exceeded rejection naming it, got %v", err)
	}
}

// Hook-dispatch tests (validator veto/accept, transformer threading, the
// no-tenant refusal) live in hook_test.go: they call Module.hookHandler
// directly, which is unexported, so they run in this package rather than
// wasmhost_test.

// TestABIVersion1RefusesV2Grants: a manifest cannot approve v2-only grants
// for a guest that declares ABI version 1 — it has no import table to
// exercise them, so approving them would be policy the guest can never act
// on.
func TestABIVersion1RefusesV2Grants(t *testing.T) {
	manifest := wasmhost.ApprovedManifest{
		ID:         "com.example.pricing",
		ABIVersion: 1,
		Provides:   []modulekit.CapabilityRef{testRef},
		Consumes:   []modulekit.CapabilityRef{testRef},
	}
	_, err := wasmhost.NewModule(context.Background(), wasmhost.ModuleConfig{
		Manifest: manifest,
		Wasm:     guestBytes(t),
		Caller:   &stubCaller{handlers: map[string]modulekit.Handler{}},
	})
	if err == nil {
		t.Fatal("an ABI v1 manifest requesting v2 grants was accepted")
	}
}

// TestConsumesWithoutACallerIsRefused: a manifest that grants outbound
// capability access must be paired with something to resolve it through.
// Silently accepting Consumes with no Caller would defer the failure to the
// guest's first host call, at a point far from the wiring mistake.
func TestConsumesWithoutACallerIsRefused(t *testing.T) {
	manifest := wasmhost.ApprovedManifest{
		ID:         "com.example.guestv2",
		ABIVersion: wasmhost.CurrentABIVersion,
		Provides:   []modulekit.CapabilityRef{v2Ref},
		Consumes:   []modulekit.CapabilityRef{{Name: "inventory.check", Version: "v1"}},
	}
	_, err := wasmhost.NewModule(context.Background(), wasmhost.ModuleConfig{
		Manifest: manifest,
		Wasm:     guestV2Bytes(t),
	})
	if err == nil {
		t.Fatal("a manifest with Consumes and no Caller was accepted")
	}
}
