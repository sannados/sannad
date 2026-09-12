package wasmhost_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sannados/sannad/internal/kernel/wasmhost"
	"github.com/sannados/sannad/pkg/modulekit"
)

// This proves the wiring in internal/app/bootstrap: a WASM guest reaches the
// kernel registry through the exact same modulekit.Module interface an
// embedded Go module implements. bootstrap.RegisterModule cannot tell the
// two apart, which is the whole point of the tier.

func validManifest() wasmhost.ApprovedManifest {
	return wasmhost.ApprovedManifest{
		ID:         "com.example.pricing",
		ABIVersion: wasmhost.CurrentABIVersion,
		Provides:   []modulekit.CapabilityRef{testRef},
		Exposed:    []modulekit.CapabilityRef{testRef},
	}
}

func TestNewModuleServesItsCapabilityThroughInstall(t *testing.T) {
	module, err := wasmhost.NewModule(context.Background(), wasmhost.ModuleConfig{
		Manifest: validManifest(),
		Wasm:     guestBytes(t),
	})
	if err != nil {
		t.Fatalf("new module: %v", err)
	}
	t.Cleanup(func() { _ = module.Stop(context.Background()) })

	reg := &capturingRegistrar{handlers: map[string]modulekit.Handler{}}
	if err := module.Install(reg); err != nil {
		t.Fatalf("install: %v", err)
	}

	handler, ok := reg.handlers[testRef.Key()]
	if !ok {
		t.Fatalf("Install did not register %s", testRef.Key())
	}

	result, err := handler(tenantCtx("acme"), calcRequest{Amount: 4})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	var resp calcResponse
	if err := json.Unmarshal(result.(modulekit.RawPayload), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Doubled != 8 {
		t.Fatalf("doubled = %d, want 8", resp.Doubled)
	}
}

// TestNewModuleDescriptorDeclaresStorageNone: a sandboxed guest cannot reach a
// database the host did not wire in, so this must never claim StorageKernel or
// StorageOwn — either would tell the tenancy audit to trust isolation the guest
// cannot provide.
func TestNewModuleDescriptorDeclaresStorageNone(t *testing.T) {
	module, err := wasmhost.NewModule(context.Background(), wasmhost.ModuleConfig{
		Manifest: validManifest(),
		Wasm:     guestBytes(t),
	})
	if err != nil {
		t.Fatalf("new module: %v", err)
	}
	t.Cleanup(func() { _ = module.Stop(context.Background()) })

	if got := module.Descriptor().Storage; got != modulekit.StorageNone {
		t.Fatalf("Storage = %q, want %q", got, modulekit.StorageNone)
	}
}

// TestWrongABIVersionIsRefused: a guest cannot claim compatibility with a host
// version it was not built against. Refused at registration, before any guest
// bytes are even compiled.
func TestWrongABIVersionIsRefused(t *testing.T) {
	manifest := validManifest()
	manifest.ABIVersion = wasmhost.CurrentABIVersion + 1

	_, err := wasmhost.NewModule(context.Background(), wasmhost.ModuleConfig{
		Manifest: manifest,
		Wasm:     guestBytes(t),
	})
	if err == nil {
		t.Fatal("a manifest declaring an unsupported ABI version was accepted")
	}
}

// TestExposedWithoutProvidedIsRefused: a manifest cannot publish a capability
// through the gateway that the guest never actually provides.
func TestExposedWithoutProvidedIsRefused(t *testing.T) {
	manifest := validManifest()
	manifest.Provides = nil

	_, err := wasmhost.NewModule(context.Background(), wasmhost.ModuleConfig{
		Manifest: manifest,
		Wasm:     guestBytes(t),
	})
	if err == nil {
		t.Fatal("a manifest with no provided capabilities was accepted")
	}
}

// TestGuestCannotEscalateItsOwnManifest: even though the approved manifest
// dictates what gets registered, this documents that the guest's own opinion
// of its capabilities (which it never gets to state — there is no such field
// in the ABI) plays no role. Only host-approved config decides.
func TestDuplicateProvidedCapabilityIsRefused(t *testing.T) {
	manifest := validManifest()
	manifest.Provides = []modulekit.CapabilityRef{testRef, testRef}

	_, err := wasmhost.NewModule(context.Background(), wasmhost.ModuleConfig{
		Manifest: manifest,
		Wasm:     guestBytes(t),
	})
	if err == nil {
		t.Fatal("a manifest providing the same capability twice was accepted")
	}
}

type capturingRegistrar struct {
	handlers map[string]modulekit.Handler
}

func (r *capturingRegistrar) RegisterCapability(ref modulekit.CapabilityRef, h modulekit.Handler) error {
	r.handlers[ref.Key()] = h
	return nil
}
