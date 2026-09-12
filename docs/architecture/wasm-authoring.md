# Writing a WASM plugin

`docs/architecture/plugin-tiers.md` explains why tier 2 (sandboxed WASM) exists and how it
fits alongside embedded and external modules. This document is the missing piece: the exact
steps to write, build, and register a real guest, in enough detail to do it without reading
`internal/kernel/wasmhost`'s source.

The ABI itself is defined and enforced in `internal/kernel/wasmhost/abi.go`; this document
does not redefine it, only walks a working guest end to end.

## The contract in one paragraph

A guest exports exactly two functions and shares its linear memory with the host:

```
sannad_alloc(size uint32) uint32      // reserve size bytes, return their offset
sannad_handle(ptr uint32, len uint32) uint64   // handle one request, return a packed (ptr, len)
```

The host writes a JSON **envelope** into memory the guest allocated via `sannad_alloc`, calls
`sannad_handle` with that offset and length, and reads the response from the `uint64` it packs
back: the high 32 bits are a pointer, the low 32 bits a length, both into the guest's own
memory. The guest never receives a Go pointer, and the host never writes anywhere the guest
did not hand it — that boundary is the whole security property tier 2 exists for.

The envelope for a capability call:

```json
{"capability": "pricing.calculator@v1", "tenant_id": "acme", "payload": { }}
```

`payload` is whatever the capability's own request type serializes to. The guest's response is
one status byte followed by a payload:

- `0x00` + JSON body → success, decoded as the capability's response type.
- `0x01` + UTF-8 text → an error, or a deliberate business rejection — both are one thing at
  this layer; the host turns it into an `error` the caller sees the same way either shape.

## A minimal guest, in Go

This is `internal/kernel/wasmhost/testdata/guest/main.go`, trimmed to the parts every guest
needs regardless of what it actually computes:

```go
//go:build wasip1

package main

import (
	"encoding/json"
	"unsafe"
)

// The host writes into memory this guest allocates; the guest's own garbage
// collector must not reclaim it while the host still holds the offset, so
// every allocation is kept alive here for the guest's lifetime.
var buffers = map[uint32][]byte{}

//go:wasmexport sannad_alloc
func alloc(size uint32) uint32 {
	buf := make([]byte, size)
	ptr := uint32(uintptr(unsafe.Pointer(&buf[0])))
	buffers[ptr] = buf
	return ptr
}

type envelope struct {
	Capability string          `json:"capability"`
	TenantID   string          `json:"tenant_id"`
	Payload    json.RawMessage `json:"payload"`
}

type request struct {
	Amount int `json:"amount"`
}

type response struct {
	Doubled int `json:"doubled"`
}

//go:wasmexport sannad_handle
func handle(ptr uint32, length uint32) uint64 {
	input := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), length)

	var env envelope
	if err := json.Unmarshal(input, &env); err != nil {
		return respond(1, []byte("malformed envelope"))
	}
	var req request
	_ = json.Unmarshal(env.Payload, &req)

	out, err := json.Marshal(response{Doubled: req.Amount * 2})
	if err != nil {
		return respond(1, []byte("encode failed"))
	}
	return respond(0, out)
}

func respond(status byte, payload []byte) uint64 {
	out := make([]byte, 0, len(payload)+1)
	out = append(out, status)
	out = append(out, payload...)
	ptr := uint32(uintptr(unsafe.Pointer(&out[0])))
	buffers[ptr] = out
	return uint64(ptr)<<32 | uint64(len(out))
}

// main is never called — see the build note below for why the module still
// needs one.
func main() {}
```

Any language that compiles to a WASI-targeted `.wasm` module and can export two functions
under those exact names, using that same linear-memory calling convention, can be a guest.
Go is shown because the kernel's own test fixtures are Go, not because the ABI requires it.

## Building it

```sh
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o guest.wasm ./guest/
```

`-buildmode=c-shared` matters more than it looks: it produces a WebAssembly **reactor**
(exports `_initialize`, not `_start`), so the runtime initializes and then returns control to
the host with `sannad_alloc`/`sannad_handle` still callable. A plain command-mode build exports
only `_start` — the module runs `main` and exits, and by the time the host tries to call
`sannad_handle` there is nothing left running to call. Blocking forever inside `main` to avoid
that either deadlocks (Go's own scheduler aborts on "all goroutines are asleep") or never
returns control to the host at all. A plugin is a library the host calls into repeatedly, not
a program that runs once — `-buildmode=c-shared` is what makes that true for a Go guest.

## Registering it

Through the public `sannad` package — this is the surface a third-party embedder actually
has, not `internal/kernel/wasmhost` directly:

```go
wasm, err := os.ReadFile("guest.wasm")
if err != nil {
    return err
}

err = app.RegisterWASM(ctx, sannad.WASMConfig{
    Manifest: sannad.WASMManifest{
        ID:         "com.example.pricing",
        ABIVersion: sannad.CurrentWASMABIVersion,
        Provides:   []modulekit.CapabilityRef{{Name: "pricing.calculator", Version: "v1"}},
        // Exposed publishes it to the external HTTP/gRPC bridge too. Leave
        // it empty to keep the capability internal-only.
    },
    Wasm: wasm,
    // The sandbox is the trust boundary, so its limits are the security
    // contract — set them deliberately rather than leaving every one at
    // its (generous) default.
    MaxMemoryPages: 256,           // 64 KiB pages; 256 = 16 MiB
    Timeout:        50 * time.Millisecond,
})
```

The manifest is **operator-approved input, not metadata read from the guest.** Nothing inside
`guest.wasm` can grant itself a capability, an outbound call, or a hook subscription by
declaring one in its own bytes — every grant here comes from the Go value the host process
was given, which is the whole reason a WASM guest is safe to run with someone else's code
inside it.

## Beyond a single capability: the ABI v2 host-call surface

A guest that only answers requests never needs anything past what's above. A guest that also
needs to call *another* capability, publish an event, or participate in a hook — the ADR 0007
surface — declares those as additional manifest grants:

```go
Manifest: sannad.WASMManifest{
    ID:         "com.example.pricing",
    ABIVersion: sannad.CurrentWASMABIVersion, // 2: this guest uses host calls
    Provides:   []modulekit.CapabilityRef{pricingRef},
    Consumes:   []modulekit.CapabilityRef{inventoryCheckRef},
    PublishesEvents: []string{"pricing.recalculated"},
},
```

Same rule as `Provides`/`Exposed`: a guest asking to call `inventory.check@v1` through the v2
import table when the manifest never listed it in `Consumes` is refused before the call
reaches anything — declared intent inside the guest is not authority. See ADR 0007 for the
full host-call ABI (`sannad_v2.capability_call`, `sannad_v2.event_publish`, and inbound hook
dispatch) and `internal/kernel/wasmhost/testdata/guestv2/main.go` for a complete worked guest
using it. `sannad.WASMConfig` also carries the v2 resource limits
(`MaxOutboundRequestBytes`, `MaxResultBytes`, `MaxCallDepth`, and so on) — all optional, all
defaulted to values generous enough for real work and far below anything that threatens the
host.

## Debugging a guest

`sannad.WASMConfig.Stdout`/`Stderr` are the one real capability the sandbox grants, and they
exist for exactly this: without them, a guest panic is silent and there is nothing to look at.

```go
Stdout: func(b []byte) { log.Printf("guest stdout: %s", b) },
Stderr: func(b []byte) { log.Printf("guest stderr: %s", b) },
```

A guest that spins forever or allocates without bound is not a hang to debug in the guest —
it is the sandbox doing exactly what it is for. `Timeout` and `MaxMemoryPages` are what stop
it; if either trips during real development rather than a deliberate test, the fix is almost
always in the guest's own loop or allocation pattern, not in loosening the limit.

## What this tier cannot do

No filesystem, no network, no ambient clock beyond what the host passes in, and no way to ask
for any of them from inside the guest — every capability a guest has is a host function wired
in deliberately, and the default for anything not wired in is "no". A module needing a real
outbound network call or a persistent connection belongs at tier 3 (external app), not tier 2;
trying to work around the sandbox from inside a WASM guest is the wrong direction to push.
