// This file is roadmap step 4 for ADR 0007: the cross-tier conformance matrix
// the earlier hook conformance suite in this package does not cover. That
// suite proves extension composition entirely in-process; this proves the
// ADR 0007 claim specifically — that a WASM guest reaching an embedded
// capability through capability_call uses the same route, the same tenant
// propagation, and produces the same result an in-process caller gets calling
// that capability directly. There is no sandbox-specific service locator to
// verify separately from the ordinary one.
package conformance_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/getkayan/kayan/core/tenant"
	"github.com/sannados/sannad/internal/kernel/bus"
	"github.com/sannados/sannad/internal/kernel/registry"
	"github.com/sannados/sannad/internal/kernel/wasmhost"
	"github.com/sannados/sannad/pkg/modulekit"
)

// embeddedEchoModule is the tier-1 provider every other tier in this matrix
// is checked against: it doubles Amount and reports the tenant it saw.
type embeddedEchoModule struct{}

type echoRequest struct {
	Amount int `json:"amount"`
}

type echoResponse struct {
	Doubled  int    `json:"doubled"`
	TenantID string `json:"tenant_id"`
}

var downstreamRef = modulekit.CapabilityRef{Name: "conformance.echo", Version: "v1"}

func (embeddedEchoModule) Descriptor() modulekit.Descriptor {
	return modulekit.Descriptor{
		ID:       "com.sannad.conformance.echo",
		Provides: []modulekit.CapabilityRef{downstreamRef},
		Storage:  modulekit.StorageNone,
	}
}

func (embeddedEchoModule) Install(reg modulekit.Registrar) error {
	return modulekit.HandleTyped(reg, downstreamRef, func(ctx context.Context, req echoRequest) (echoResponse, error) {
		return echoResponse{Doubled: req.Amount * 2, TenantID: tenant.IDFromContext(ctx)}, nil
	})
}

func (embeddedEchoModule) Start(context.Context) error { return nil }
func (embeddedEchoModule) Stop(context.Context) error  { return nil }

var _ modulekit.Module = embeddedEchoModule{}

// crossTierGuestBytes loads the same ABI v2 fixture the wasmhost package's own
// tests compile against — a second copy would prove nothing an existing one
// does not, and a fixture this suite maintained independently would drift
// from the ABI it is meant to be checked against.
func crossTierGuestBytes(t *testing.T) []byte {
	t.Helper()
	path := "../wasmhost/testdata/guestv2.wasm"
	wasm, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (rebuild with GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o %s ../wasmhost/testdata/guestv2/)", path, path, path)
	}
	return wasm
}

var wasmProvidedRef = modulekit.CapabilityRef{Name: "conformance.wasm_caller", Version: "v1"}

func crossTierCtx(id string) context.Context {
	return tenant.WithTenantID(context.Background(), id)
}

// TestWASMToEmbeddedUsesTheOrdinaryRegistry: a WASM guest's capability_call
// resolves an embedded module's capability through the same bus.Call an
// in-process caller would use, and the two get the same answer for the same
// tenant. This is the ADR's "one route, one policy check" claim exercised
// against a real compiled guest rather than asserted in a comment.
func TestWASMToEmbeddedUsesTheOrdinaryRegistry(t *testing.T) {
	reg := registry.New()
	if err := reg.RegisterModule(embeddedEchoModule{}); err != nil {
		t.Fatalf("register embedded module: %v", err)
	}
	b := bus.New(reg)

	wasmModule, err := wasmhost.NewModule(context.Background(), wasmhost.ModuleConfig{
		Manifest: wasmhost.ApprovedManifest{
			ID:         "com.sannad.conformance.wasmcaller",
			ABIVersion: wasmhost.CurrentABIVersion,
			Provides:   []modulekit.CapabilityRef{wasmProvidedRef},
			Consumes:   []modulekit.CapabilityRef{downstreamRef},
		},
		Wasm:    crossTierGuestBytes(t),
		Caller:  b,
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("new wasm module: %v", err)
	}
	if err := reg.RegisterModule(wasmModule); err != nil {
		t.Fatalf("register wasm module: %v", err)
	}

	// The direct, in-process answer this matrix is checked against.
	direct, err := b.Call(crossTierCtx("acme"), downstreamRef, echoRequest{Amount: 7})
	if err != nil {
		t.Fatalf("direct call: %v", err)
	}
	directResp := direct.(echoResponse)

	// The same capability, reached through a WASM guest's capability_call.
	callPayload, err := json.Marshal(echoRequest{Amount: 7})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	viaWASM, err := b.Call(crossTierCtx("acme"), wasmProvidedRef, map[string]any{
		"call_capability": downstreamRef.Key(),
		"call_payload":    json.RawMessage(callPayload),
	})
	if err != nil {
		t.Fatalf("call through wasm: %v", err)
	}

	var wasmResp echoResponse
	if err := json.Unmarshal(viaWASM.(modulekit.RawPayload), &wasmResp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if wasmResp != directResp {
		t.Fatalf("wasm-relayed result %+v does not match the direct call %+v", wasmResp, directResp)
	}
}

// TestWASMToEmbeddedPreservesTheRealCallersTenant: the tenant the embedded
// module sees when reached through a WASM guest is the real caller's tenant,
// not one the guest could have named — there is no field in the wire
// envelope for it to supply one in.
func TestWASMToEmbeddedPreservesTheRealCallersTenant(t *testing.T) {
	reg := registry.New()
	if err := reg.RegisterModule(embeddedEchoModule{}); err != nil {
		t.Fatalf("register embedded module: %v", err)
	}
	b := bus.New(reg)

	wasmModule, err := wasmhost.NewModule(context.Background(), wasmhost.ModuleConfig{
		Manifest: wasmhost.ApprovedManifest{
			ID:         "com.sannad.conformance.wasmcaller2",
			ABIVersion: wasmhost.CurrentABIVersion,
			Provides:   []modulekit.CapabilityRef{wasmProvidedRef},
			Consumes:   []modulekit.CapabilityRef{downstreamRef},
		},
		Wasm:    crossTierGuestBytes(t),
		Caller:  b,
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("new wasm module: %v", err)
	}
	if err := reg.RegisterModule(wasmModule); err != nil {
		t.Fatalf("register wasm module: %v", err)
	}

	callPayload, _ := json.Marshal(echoRequest{Amount: 1})
	result, err := b.Call(crossTierCtx("real-tenant"), wasmProvidedRef, map[string]any{
		"call_capability": downstreamRef.Key(),
		"call_payload":    json.RawMessage(callPayload),
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var resp echoResponse
	if err := json.Unmarshal(result.(modulekit.RawPayload), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.TenantID != "real-tenant" {
		t.Fatalf("embedded module saw tenant %q, want real-tenant", resp.TenantID)
	}
}

// TestWASMRefusesToCallWhatItWasNotApprovedFor: the same registry, the same
// downstream module, but no grant. ADR 0007 rule 1 — guest-declared intent is
// not authority — proven against the real registry rather than the wasmhost
// package's own stand-in caller.
func TestWASMRefusesToCallWhatItWasNotApprovedFor(t *testing.T) {
	reg := registry.New()
	if err := reg.RegisterModule(embeddedEchoModule{}); err != nil {
		t.Fatalf("register embedded module: %v", err)
	}
	b := bus.New(reg)

	wasmModule, err := wasmhost.NewModule(context.Background(), wasmhost.ModuleConfig{
		Manifest: wasmhost.ApprovedManifest{
			ID:         "com.sannad.conformance.wasmcaller3",
			ABIVersion: wasmhost.CurrentABIVersion,
			Provides:   []modulekit.CapabilityRef{wasmProvidedRef},
			// No Consumes: this guest was never granted downstreamRef.
		},
		Wasm:    crossTierGuestBytes(t),
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("new wasm module: %v", err)
	}
	if err := reg.RegisterModule(wasmModule); err != nil {
		t.Fatalf("register wasm module: %v", err)
	}

	callPayload, _ := json.Marshal(echoRequest{Amount: 1})
	_, err = b.Call(crossTierCtx("acme"), wasmProvidedRef, map[string]any{
		"call_capability": downstreamRef.Key(),
		"call_payload":    json.RawMessage(callPayload),
	})
	if err == nil {
		t.Fatal("a WASM guest reached a capability its manifest never granted")
	}
}
