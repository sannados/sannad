# ADR 0003: Extension Points and Hooks

- Status: Accepted
- Date: 2026-08-09

## Context

The current kernel registry models one relationship: a module *provides* a capability
and another module *consumes* it. That is enough for module A to call module B.

It is not enough for an ecosystem. The reason WordPress has hundreds of thousands of
plugins is not its API quality; it is that any plugin can hook into flows it does not
own. Shopify's equivalent is Functions plus metafields. Odoo's is `_inherit`
monkey-patching, which works and is also the direct cause of its upgrade fragility.

An ecosystem plugin needs to do things like:

- run logic before or after an entity is created or updated,
- veto an operation (validation, credit limit, fraud check),
- contribute a field to an entity it does not own,
- add a step to a workflow,
- transform a value in a pipeline (pricing, tax, discount).

None of these are expressible as "call capability X". They require the *owner* of a
flow to declare points where others may participate.

Retrofitting this after twenty modules exist is extremely expensive, because every
existing flow must be re-opened. It must be designed now, even if implementation is
staged.

### The typed handler problem

`modulekit.Handler` is currently `func(ctx, request any) (any, error)`.

This works in-process, where both sides are Go and share types. It cannot cross a
tier boundary: `any` is not serializable without knowing its concrete type, so the
same hook cannot be implemented by a WASM module or a remote app. Since tier
symmetry (ADR 0001) is a core architectural property, `any` on cross-tier paths
blocks the design.

It is also poor DX. Every handler begins with a type assertion that can fail at
runtime, which is precisely the class of error a Go-based system should eliminate.

## Decision

### 1. Hooks are first-class registry citizens

The kernel gains a hook registry alongside the capability registry. Where a
capability has exactly one provider, a hook point has **many subscribers**.

A module declares the hook points it *offers*, in its descriptor, alongside the
capabilities it provides and consumes. Offering a hook point is a public API
commitment, versioned like any contract.

### 2. Hook kinds

Three kinds, distinguished by what the kernel does with the return value:

- **Observer** — notified after the fact, return value ignored, failure cannot block
  the operation. May be dispatched asynchronously. (`after_contact_created`)
- **Validator** — may veto. Returns accept or reject with reason. Runs synchronously
  inside the operation, before commit. (`before_invoice_post`)
- **Transformer** — receives a value and returns a possibly modified value. Runs in
  a defined order, output of one feeding the next. (`compute_line_price`)

Mixing these is a common design failure: an "observer" that can secretly block, or a
transformer with unclear ordering, produces systems nobody can reason about.

### 3. Ordering and determinism

- Subscribers declare a numeric priority. Ties break on module ID, so ordering is
  **deterministic** and does not depend on registration order or map iteration.
- Transformer chains execute in priority order.
- Validators all run; results are collected. A single veto rejects, and the caller
  receives every rejection reason rather than just the first.

### 4. Failure policy is declared, never implied

Every hook point declares what happens when a subscriber fails or times out:

- `fail_closed` — subscriber failure aborts the operation. For correctness-critical
  hooks such as credit checks.
- `fail_open` — subscriber failure is logged and skipped. For enrichment and
  non-essential logic.

Every synchronous hook point declares a **timeout**. There is no unbounded wait on
third-party code, in-process or remote.

### 5. Typed contracts on cross-tier paths

`Handler` with `any` remains available for **in-process, same-binary** convenience,
but every capability or hook that can be served across tiers is defined by a
**protobuf message pair** (request and response).

The SDK provides generic typed wrappers so module authors write:

```go
modulekit.HandleTyped(reg, ref, func(ctx context.Context, req *crmv1.GetContactRequest) (*crmv1.GetContactResponse, error) {
    // no type assertion, no marshalling by hand
})
```

The wrapper handles decoding, encoding, and dispatch. In-process calls between Go
modules can skip serialization entirely and pass the typed value directly; only
cross-tier calls pay the marshalling cost.

This is what makes one hook contract implementable by all three tiers.

### 6. Tier-agnostic dispatch

A hook subscription records *what* to invoke, not *how*. The dispatcher resolves the
tier:

- embedded Go — direct call,
- WASM — sandboxed invocation with memory and time limits,
- external app — gRPC or HTTP callback honouring the declared timeout and failure
  policy.

The module offering the hook point neither knows nor cares which tier answers.

### 7. Staging

- **Phase 1**: hook registry, three hook kinds, priority ordering, failure policy,
  timeouts, typed contracts, embedded Go and external HTTP/gRPC dispatch.
- **Phase 2**: WASM dispatch via `wazero`. No contract changes required, because
  contracts were designed tier-agnostic from the start.

## Consequences

- Third parties can extend flows they do not own, without forking or patching core.
  This is the central ecosystem-enabling property and the main answer to Odoo's
  `_inherit`.
- Hook points become public API. Removing or changing one is a breaking change and
  must follow contract versioning discipline.
- Synchronous hooks put third-party code in the request path. Timeouts and declared
  failure policy bound the damage; WASM later removes the network component.
- Debuggability requires investment: with many subscribers, "why did this operation
  fail" needs tracing that shows which subscriber vetoed and why. This should be
  built alongside the registry, not after.
- Typed handlers mean more protobuf authoring up front. Codegen should reduce that
  to a single entity definition producing contract, handler skeleton, and client
  (see ADR 0004 consequences).
- The `any`-typed handler path must be clearly documented as in-process only, or
  authors will use it and later discover their module cannot be ported to WASM.

## Alternatives considered

- **Capabilities only, no hooks.** Smallest kernel. Rejected: it forces every
  extension to be a fork or a core change, which is the failure mode this project
  exists to avoid.
- **WordPress-style untyped global hook bus.** Maximum flexibility and a proven
  ecosystem driver. Rejected: no type safety, no ordering guarantees, no failure
  policy, and impossible to serve across tiers.
- **Odoo-style inheritance and method override.** Extremely expressive for the
  author. Rejected: it is precisely the mechanism that makes Odoo upgrades break, and
  it is not expressible for non-Go or sandboxed tiers.
- **Async-only extension (events, no synchronous hooks).** Removes all availability
  risk from the request path. Rejected: validation and vetoing are inherently
  synchronous, and refusing to support them pushes authors back toward forking.
