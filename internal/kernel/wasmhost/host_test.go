package wasmhost_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/getkayan/kayan/core/tenant"
	"github.com/sannados/sannad/internal/kernel/wasmhost"
	"github.com/sannados/sannad/pkg/modulekit"
)

// These run against a real WebAssembly module compiled from testdata/guest,
// not a hand-written byte array. A fixture that never went through a compiler
// would prove nothing about whether the ABI is implementable — which is the
// first thing a tier design has to establish.
//
// Rebuild with:
//
//	GOOS=wasip1 GOARCH=wasm go build -o testdata/guest.wasm ./testdata/guest/

func guestBytes(t *testing.T) []byte {
	t.Helper()
	wasm, err := os.ReadFile("testdata/guest.wasm")
	if err != nil {
		t.Fatalf("read guest: %v (rebuild with GOOS=wasip1 GOARCH=wasm go build -o testdata/guest.wasm ./testdata/guest/)", err)
	}
	return wasm
}

func load(t *testing.T, config wasmhost.Config) *wasmhost.Plugin {
	t.Helper()
	if config.ModuleID == "" {
		config.ModuleID = "com.example.guest"
	}
	if config.Wasm == nil {
		config.Wasm = guestBytes(t)
	}
	// The Go toolchain's wasip1 output carries a large runtime, so the test
	// budget is looser than a production default. The limits still apply — the
	// tests below prove they are enforced.
	if config.Timeout == 0 {
		config.Timeout = 5 * time.Second
	}

	plugin, err := wasmhost.Load(context.Background(), config)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Cleanup(func() { _ = plugin.Close(context.Background()) })
	return plugin
}

var testRef = modulekit.CapabilityRef{Name: "pricing.calculator", Version: "v1"}

type calcRequest struct {
	Amount int    `json:"amount"`
	Reject bool   `json:"reject"`
	Spin   bool   `json:"spin"`
	Grow   bool   `json:"grow"`
	Echo   string `json:"echo"`
}

type calcResponse struct {
	Doubled  int    `json:"doubled"`
	TenantID string `json:"tenant_id"`
	Echo     string `json:"echo"`
}

func tenantCtx(id string) context.Context {
	return tenant.WithTenantID(context.Background(), id)
}

// TestGuestServesACapability is the tier's central claim: a capability written
// in a sandboxed guest is invoked through the same modulekit.Handler as an
// embedded one, and the caller cannot tell.
func TestGuestServesACapability(t *testing.T) {
	plugin := load(t, wasmhost.Config{})
	handler := plugin.Handler(testRef, wasmhost.JSONCodec{})

	result, err := handler(tenantCtx("acme"), calcRequest{Amount: 21})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	// The response arrives as RawPayload, exactly as it does from the HTTP
	// bridge, and decodes into the caller's contract type through the same path.
	raw, ok := result.(modulekit.RawPayload)
	if !ok {
		t.Fatalf("got %T, want modulekit.RawPayload", result)
	}
	var resp calcResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Doubled != 42 {
		t.Fatalf("doubled = %d, want 42", resp.Doubled)
	}

	// A transport has already encoded this request. It must cross the second
	// boundary into WASM without being encoded as a byte string.
	result, err = handler(tenantCtx("acme"), modulekit.RawPayload(`{"amount":21}`))
	if err != nil {
		t.Fatalf("raw handler: %v", err)
	}
	var rawResponse calcResponse
	if err := json.Unmarshal(result.(modulekit.RawPayload), &rawResponse); err != nil {
		t.Fatalf("decode raw response: %v", err)
	}
	if rawResponse.Doubled != 42 {
		t.Fatalf("raw doubled = %d, want 42", rawResponse.Doubled)
	}
}

// TestGuestReceivesTheContextTenant. A Go context does not cross into a guest,
// so the tenant is written into the envelope by the host. It must come from the
// request context and not from anything the caller put in the payload.
func TestGuestReceivesTheContextTenant(t *testing.T) {
	plugin := load(t, wasmhost.Config{})
	handler := plugin.Handler(testRef, wasmhost.JSONCodec{})

	result, err := handler(tenantCtx("acme"), calcRequest{Amount: 1})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	var resp calcResponse
	if err := json.Unmarshal(result.(modulekit.RawPayload), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.TenantID != "acme" {
		t.Fatalf("guest saw tenant %q, want acme", resp.TenantID)
	}
}

// TestNoTenantIsRefused: invoking a guest with no tenant in context would give
// it an empty tenant to act for, which is the shape of a cross-tenant write.
func TestNoTenantIsRefused(t *testing.T) {
	plugin := load(t, wasmhost.Config{})
	handler := plugin.Handler(testRef, wasmhost.JSONCodec{})

	if _, err := handler(context.Background(), calcRequest{Amount: 1}); err == nil {
		t.Fatal("a guest was invoked with no tenant in context")
	}
}

// TestGuestRejectionIsNotAMalfunction. The status byte separates a business
// refusal from a crash; without it a fail-closed hook could not tell a
// legitimate veto from a guest that trapped.
func TestGuestRejectionIsNotAMalfunction(t *testing.T) {
	plugin := load(t, wasmhost.Config{})
	handler := plugin.Handler(testRef, wasmhost.JSONCodec{})

	_, err := handler(tenantCtx("acme"), calcRequest{Reject: true})
	if err == nil {
		t.Fatal("a rejecting guest reported success")
	}
	if !errors.Is(err, modulekit.ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got %v", err)
	}
	// And it must not look like a guest failure, which is what an operator would
	// investigate as a bug.
	if errors.Is(err, wasmhost.ErrGuestFailed) {
		t.Fatal("a deliberate rejection was reported as a guest malfunction")
	}
}

// TestRunawayGuestIsInterrupted is the guarantee the embedded tier cannot make.
//
// A tier-1 subscriber that ignores its context leaves a goroutine the kernel can
// only abandon. A guest in an infinite loop is stopped, because the runtime owns
// its execution — this is the main reason the WASM tier exists.
func TestRunawayGuestIsInterrupted(t *testing.T) {
	plugin := load(t, wasmhost.Config{Timeout: 100 * time.Millisecond})
	handler := plugin.Handler(testRef, wasmhost.JSONCodec{})

	start := time.Now()
	_, err := handler(tenantCtx("acme"), calcRequest{Spin: true})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("an infinite loop in a guest returned successfully")
	}
	if !errors.Is(err, wasmhost.ErrGuestTimeout) {
		t.Fatalf("expected ErrGuestTimeout, got %v", err)
	}
	// The bound must be real, not merely reported after the fact.
	if elapsed > 3*time.Second {
		t.Fatalf("the guest ran for %s despite a 100ms limit", elapsed)
	}
}

// TestCallerDeadlineBoundsTheGuest: a guest must not outlive the request it is
// serving, even when its own limit is looser.
func TestCallerDeadlineBoundsTheGuest(t *testing.T) {
	plugin := load(t, wasmhost.Config{Timeout: 30 * time.Second})
	handler := plugin.Handler(testRef, wasmhost.JSONCodec{})

	ctx, cancel := context.WithTimeout(tenantCtx("acme"), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := handler(ctx, calcRequest{Spin: true}); err == nil {
		t.Fatal("the guest outlived the caller's deadline")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the guest ran %s past a 150ms caller deadline", elapsed)
	}
}

// TestMemoryLimitIsEnforced. A guest allocating in a loop must trap rather than
// exhausting the host.
func TestMemoryLimitIsEnforced(t *testing.T) {
	plugin := load(t, wasmhost.Config{
		MaxMemoryPages: 512, // 32 MiB, enough for the Go runtime and not much more
		Timeout:        10 * time.Second,
	})
	handler := plugin.Handler(testRef, wasmhost.JSONCodec{})

	if _, err := handler(tenantCtx("acme"), calcRequest{Grow: true}); err == nil {
		t.Fatal("a guest allocating without bound reported success")
	}
}

// TestGuestsDoNotShareState. A fresh instance per call is what stops one
// tenant's invocation leaving data another tenant's invocation can read. A
// long-lived instance would be faster and would make that a matter of guest
// discipline.
func TestGuestsDoNotShareState(t *testing.T) {
	plugin := load(t, wasmhost.Config{})
	handler := plugin.Handler(testRef, wasmhost.JSONCodec{})

	first, err := handler(tenantCtx("acme"), calcRequest{Amount: 5, Echo: "acme-secret"})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	var firstResp calcResponse
	_ = json.Unmarshal(first.(modulekit.RawPayload), &firstResp)
	if firstResp.Echo != "acme-secret" {
		t.Fatalf("setup: echo = %q", firstResp.Echo)
	}

	// A second tenant sends nothing in Echo. If the guest retained state between
	// invocations, the first tenant's value would come back.
	second, err := handler(tenantCtx("other"), calcRequest{Amount: 5})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	var secondResp calcResponse
	_ = json.Unmarshal(second.(modulekit.RawPayload), &secondResp)

	if secondResp.Echo != "" {
		t.Fatalf("a second tenant read %q from the previous invocation", secondResp.Echo)
	}
	if secondResp.TenantID != "other" {
		t.Fatalf("guest saw tenant %q, want other", secondResp.TenantID)
	}
}

// TestUnportableContractIsRefused: the portability check runs before encoding,
// so a contract that cannot cross a tier boundary is reported as the contract
// error it is rather than as a confusing marshalling failure.
//
// Until this tier existed nothing exercised AssertPortable against a real
// boundary.
func TestUnportableContractIsRefused(t *testing.T) {
	plugin := load(t, wasmhost.Config{})
	handler := plugin.Handler(testRef, wasmhost.JSONCodec{})

	type withFunc struct {
		Amount   int
		Validate func(int) bool
	}

	_, err := handler(tenantCtx("acme"), withFunc{Amount: 1})
	if !errors.Is(err, modulekit.ErrNotPortable) {
		t.Fatalf("expected ErrNotPortable, got %v", err)
	}
}

// TestModuleMissingTheABIIsRefusedAtLoad, not on the first request, so an
// operator installing a broken plugin learns immediately.
func TestModuleMissingTheABIIsRefusedAtLoad(t *testing.T) {
	// A valid WebAssembly module that exports nothing: (module)
	empty := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

	_, err := wasmhost.Load(context.Background(), wasmhost.Config{
		ModuleID: "com.example.broken",
		Wasm:     empty,
	})
	if !errors.Is(err, wasmhost.ErrGuestContract) {
		t.Fatalf("expected ErrGuestContract, got %v", err)
	}
}

func TestInvalidBytesAreRefused(t *testing.T) {
	_, err := wasmhost.Load(context.Background(), wasmhost.Config{
		ModuleID: "com.example.garbage",
		Wasm:     []byte("this is not webassembly"),
	})
	if err == nil {
		t.Fatal("arbitrary bytes loaded as a WASM module")
	}
}
