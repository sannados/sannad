# Plugin tiers and cross-tier communication

For a practical, step-by-step guide to writing, building, and registering a real WASM
guest — the ABI contract, the exact build command, and why it needs `-buildmode=c-shared`
— see `docs/architecture/wasm-authoring.md`. This document is about the architecture and
why three tiers exist at all; that one is about actually shipping tier 2.

Status of each sample is marked. **Built** = runs today. **Designed** = ADR-committed,
not implemented; the code is what it will look like, not what compiles now.

| Tier | Isolation | Latency | Status |
|---|---|---|---|
| 1. Embedded Go | none | in-process | **built** |
| 2. Sandboxed WASM (`wazero`) | memory, time, syscalls | in-process | **built** for provided capabilities; outbound calls/hooks remain |
| 3. External app | process + network | network hop | **partly built** (inbound HTTP/gRPC and events exist; serving a capability remotely does not) |

The central claim, quoted from ADR 0001:

> The same capability lives at any tier. A `parties.reader@v1` capability can be
> served by an embedded Go module, a WASM module, or a remote Salesforce-backed
> app, and callers cannot tell the difference.

Every sample below exists to show that one property. The caller code is identical
in all six pairings; only registration differs.

---

## The two contracts everything speaks

Both are already real:

```go
// A capability: request/response, one owner, versioned.
type CapabilityRef struct{ Name, Version string }
type Handler func(ctx context.Context, request any) (any, error)

// A hook: many participants on a flow someone else owns.
type HookPoint struct {
    Ref         HookRef
    Kind        HookKind      // observer | validator | transformer
    Policy      FailurePolicy // fail_open | fail_closed
    Timeout     time.Duration
    Description string
}
```

`any` is the *dispatch* signature, not the authored one. Module authors write typed
handlers and the SDK adapts them — this is what makes the same code portable across
tiers, because the typed value is what gets serialized at a boundary.

```go
// Authored surface. Built today.
modulekit.HandleTyped(func(ctx context.Context, req ListParties) (PartyList, error) {
    // ...
})
```

---

## Tier 1 — embedded Go (built)

A module is a descriptor plus lifecycle. It declares what it provides, consumes,
and opens for extension.

```go
package parties

const ModuleID = "com.acme.parties"

var Reader = modulekit.CapabilityRef{Name: "parties.reader", Version: "v1"}

// A hook point is a public API commitment: removing it is a breaking change.
var HookBeforeCreate = modulekit.HookRef{Name: "parties.before_create", Version: "v1"}

type Module struct{ store modulekit.Store }

func (m *Module) Descriptor() modulekit.Descriptor {
    return modulekit.Descriptor{
        ID:       ModuleID,
        Kind:     "embedded",
        Provides: []modulekit.CapabilityRef{Reader},
        Offers: []modulekit.HookPoint{{
            Ref:         HookBeforeCreate,
            Kind:        modulekit.KindValidator,
            Policy:      modulekit.FailClosed,
            Timeout:     time.Second,
            Description: "Veto a party before it is created.",
        }},
    }
}

func (m *Module) Install(reg modulekit.Registrar) error {
    return reg.RegisterCapability(Reader,
        modulekit.HandleTyped(func(ctx context.Context, req ListParties) (PartyList, error) {
            var rows []Party
            // Tenant is never named: it travels in ctx and the adapter scopes the query.
            if err := m.store.Find(ctx, &rows, modulekit.Where("kind", req.Kind)); err != nil {
                return PartyList{}, err
            }
            return PartyList{Parties: rows}, nil
        }))
}
```

## Tier 2 — sandboxed WASM (built for provided capabilities)

Same two contracts, different registration. The author writes a guest in any
language that targets WASM; the kernel loads bytes and registers a handler that
dispatches into the sandbox.

```go
// Host side — bootstrap loads approved bytes and registers an ordinary module.
err := app.RegisterWASM(ctx, sannad.WASMConfig{
    Manifest: sannad.WASMManifest{
        ID:         "com.community.pricing",
        ABIVersion: sannad.CurrentWASMABIVersion,
        Provides:   []modulekit.CapabilityRef{pricing.Calculator},
        Exposed:    []modulekit.CapabilityRef{pricing.Calculator},
    },
    Wasm: bytes,
    // The sandbox is the trust boundary, so its limits are the security contract.
    MaxMemoryPages: 256,
    Timeout:        50 * time.Millisecond,
})
```

The manifest above is operator-approved input, not a declaration trusted from inside
the guest. Registration is identical from the kernel's point of view — the registry
stores a `Handler`, and it does not know or care that the handler crosses into a sandbox.

```go
// Built. The adapter the host registers.
func (p *Plugin) handler(ref modulekit.CapabilityRef) modulekit.Handler {
    return func(ctx context.Context, request any) (any, error) {
        payload, err := codec.Encode(request)   // serializable contract required here
        if err != nil {
            return nil, err
        }
        // Tenant and caller are re-established inside the guest from explicit
        // fields, because a Go context does not cross the WASM boundary.
        out, err := p.invoke(ctx, ref.Key(), payload, tenantAndCallerFrom(ctx))
        if err != nil {
            return nil, err
        }
        return codec.Decode(ref, out)
    }
}
```

**Why this is the tier that fixes a known flaw.** A tier-1 subscriber that ignores
its context leaks a goroutine — the kernel cannot kill it, only abandon it. WASM
can actually interrupt the guest, because the runtime owns its execution.

## Tier 3 — external app (partly built)

No kernel code, any language, network hop. Reaches the kernel through the gateway;
receives events through a subscription.

```python
# External app calling in. Designed transport, existing capability contract.
resp = requests.post(
    "https://kernel.example.com/api/v1/capabilities/parties.reader@v1",
    headers={"Authorization": f"Bearer {token}"},   # Kayan-issued; tenant is IN the token
    json={"kind": "customer"},
)
```

The tenant is never a request field. It is resolved from the token at the edge — a
body field would let a caller name someone else's tenant.

For an external app to *serve* a capability, the bridge registers a handler that
forwards:

```go
// Designed. Same shape as the WASM adapter — this is the point.
func (b *Bridge) handler(ref modulekit.CapabilityRef, endpoint string) modulekit.Handler {
    return func(ctx context.Context, request any) (any, error) {
        payload, err := codec.Encode(request)
        if err != nil {
            return nil, err
        }
        out, err := b.post(ctx, endpoint, payload, tenantAndCallerFrom(ctx))
        if err != nil {
            // A remote tier fails in ways in-process code does not. The caller
            // sees a capability error, not an HTTP status.
            // A dedicated sentinel for "the tier serving this is unreachable"
            // does not exist yet; it arrives with the bridge.
            return nil, fmt.Errorf("capability %s unavailable: %w", ref.Key(), err)
        }
        return codec.Decode(ref, out)
    }
}
```

---

## The six pairings

Here is the actual answer to "how do they communicate": **there are not six
mechanisms. There are two, and the tier does not appear in either.**

Every caller writes this, regardless of where it or the callee lives:

```go
result, err := bus.Call(ctx, parties.Reader, ListParties{Kind: "customer"})
```

The registry resolved `parties.Reader` to *a* `Handler`. Whether that handler runs
a Go function, enters a WASM sandbox, or posts to Frankfurt is decided at
registration and invisible at the call site.

| Pairing | Call path | Cost |
|---|---|---|
| internal ↔ internal | direct func call | none |
| internal ↔ sandboxed | encode, sandbox enter, decode | serialization + sandbox |
| internal ↔ external | encode, HTTP, decode | serialization + network |
| sandboxed ↔ internal | host function, decode | serialization |
| sandboxed ↔ sandboxed | guest, host, guest | two crossings |
| sandboxed ↔ external | host function, HTTP | serialization + network |
| external ↔ internal | gateway, dispatch | network |
| external ↔ external | gateway, bridge, HTTP | two network hops |

Two things follow, and both are real constraints rather than caveats:

**Sandboxed↔sandboxed and external↔external always route through the kernel.** They
never hold a direct reference to each other. That is deliberate: the registry is
where capability grants, tenant propagation, and versioning are enforced, so a
direct edge between two plugins would bypass all three.

**The cost column is the honest part.** Callers cannot tell the tiers apart
*semantically*, but they absolutely can tell them apart in latency. A hook on a
hot path with a 50ms external subscriber is a different system than one with an
in-process subscriber. This is why ADR 0001 keeps three tiers rather than
collapsing to the safest one.

---

## Events: the other direction

Capabilities are request/response and have one owner. Events are broadcast and
have many listeners, which is how a plugin reacts to something it does not own.

```go
// Tier 1 publishes. It does not know who listens, or at which tier.
//
// Through events.Publisher, not the bus: Bus has only Call. A module receives a
// publisher at construction, as examples/embedded/crm does.
publisher.Publish(ctx, "sannad.parties.created", PartyCreated{ID: p.ID, Kind: p.Kind})
```

```go
// Tier 1 consumes. Idempotent because delivery is at-least-once — the broker
// WILL hand you this event twice.
modulekit.Idempotent(ctx, store, "billing", evt.ID, func(tx modulekit.Store) error {
    return tx.Create(ctx, &CustomerView{...})
})
```

```python
# Tier 3 consumes the same event, over a webhook. Same idempotency requirement,
# and the app must implement it itself — the SDK helper is not available here.
@app.post("/events/party-created")
def on_party_created(evt):
    if already_processed(evt["id"]):   # at-least-once applies across the network too
        return {"ok": True}
```

A tier-1 module holding a projection of another module's data is not a workaround —
ADR 0004 forbids reading another module's tables, so the copy built from events *is*
the supported path. `modulekit.Project` handles the out-of-order and rebuild cases.

---

## What is enforced, and what is only stated

Honest split, because the difference matters:

**Enforced by a test that fails when broken.** Modules compile against `modulekit`
only, never `internal/` (AST check). The SDK carries no engine imports and no engine
struct tags. An extension attaches to a host flow with no shared code and no edit to
the host (conformance suite). Hook ordering, timeouts, and failure policy.

That contracts are *serializable*, and so survive a tier change.
`modulekit.AssertPortable` refuses a payload holding a `func`, a channel, an
`unsafe.Pointer`/`uintptr`, or unexported state, walking nested structs, slices and
maps to name the exact member. The conformance suite runs it against the host
fixture's real hook payloads, and both negative controls — a `func` field and a
hidden field on a live payload — fail the suite.

The unexported-field case is the one worth knowing about: it does not fail to
cross a boundary, it crosses with the field missing. In-process it works perfectly.

**Stated but unenforced.** Interfaces in a payload pass the check, because the
concrete type is what has to be portable and is not knowable at declaration time.
That gap is real and is the reason the WASM tier will need a codec-level check too.
