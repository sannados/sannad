# ADR 0001: Module Tiers and Trust Model

- Status: Accepted
- Date: 2026-08-09

## Context

Sannad OS aims to be a Business Operating System with a large plugin ecosystem,
competing with Odoo on developer experience and with the Shopify app ecosystem on
breadth. Two forces pull in opposite directions:

- **Performance and DX** favour in-process Go modules: no serialization, no network
  hop, compile-time type safety, ordinary Go tooling and debugging.
- **Safety and ecosystem scale** favour isolation: an open marketplace cannot assume
  every plugin author is trustworthy or competent.

In-process Go code has no isolation boundary. A community module can call
`os.Exit`, spawn goroutines that never stop, read process memory including session
secrets, monkey-patch nothing but still bypass the capability registry by importing
packages directly, and crash the host process. No amount of code review at install
time changes this.

Odoo took the "everything is in-process" path. Its marketplace apps are Python
modules that patch core models via `_inherit`. The result is a large ecosystem with
chronic upgrade breakage and no meaningful security boundary for hosted offerings.

Shopify took the opposite path. No third-party code runs in their process at all;
apps are remote, and latency-critical extension runs as sandboxed WASM (Shopify
Functions). The result is a very large ecosystem with a strong safety story, but
third-party code can never achieve in-process performance.

## Decision

Sannad OS defines **three module tiers**, distinguished by trust and isolation, all
speaking the **same capability and hook contracts**.

### Tier 1: Embedded Go modules (full trust, in-process)

- Written in Go, compiled into the kernel binary.
- Compile against `pkg/modulekit` only, never `internal/`.
- Direct in-process handler dispatch. No serialization on the hot path.
- **Anyone may author and publish these.** Self-hosted operators may install any
  embedded module they choose, at their own risk, exactly as they would add any Go
  dependency to their own binary.
- **Sannad Cloud (future SaaS) will run only published and verified embedded
  modules.** Verification is a registry-level gate, not a runtime sandbox.

### Tier 2: Sandboxed WASM modules (partial trust, in-process, isolated)

- Compiled to WebAssembly, executed in-process via a pure-Go runtime (`wazero`, so
  no CGO).
- Memory-limited, time-limited, no ambient filesystem or network access. Host
  functions are the only capability surface.
- Intended for latency-critical extension by untrusted authors: pricing rules,
  validation, approval logic, field derivation.
- Implemented for capabilities the guest provides. Contracts must not assume a Go
  implementation; outbound host calls and hook subscriptions remain later ABI work.

**Implementation note, 2026-08-22.** The tier is now implemented for capabilities a
sandboxed module provides: approved manifests, bootstrap registration, lifecycle cleanup,
tenant-preserving dispatch, HTTP/gRPC exposure, memory/time limits, and restricted WASI.
Guest-to-host capability/event calls and hook subscriptions remain later ABI work. This
updates implementation status, not the trust-model decision.

**Design note, 2026-08-26.** [ADR 0007](0007-sandbox-permissions-and-host-calls.md)
now governs that later work: ABI v1 remains supported, ABI v2 adds operator-approved
outbound grants and host calls, and every synchronous call still resolves through the
ordinary capability registry.

### Tier 3: External apps (low trust, out-of-process)

- Any language. Deployed and operated independently of the kernel.
- Communicate through the gateway (ADR 0002) and the event bus.
- Never link against kernel code and never touch module internals.
- The default tier for marketplace integrations that do not need sub-millisecond
  latency.

### Trust model summary

| Tier | Isolation | Latency | Who may publish | Runs on Sannad Cloud |
|---|---|---|---|---|
| Embedded Go | None | In-process | Anyone | Only if verified |
| WASM | Strong (memory, time, syscalls) | In-process | Anyone | Yes |
| External app | Process and network | Network hop | Anyone | Yes |

### Publishing and verification

The marketplace is open in the npm and Docker Hub sense: anyone may publish, and
self-hosted operators decide what to trust. The verification pipeline gates only
what Sannad Cloud will execute in its own infrastructure.

**Amended 2026-08-20.** The tier table above describes *isolation*, and that was read
in practice as though it also described *authorship* — as though embedded meant
first-party. It does not, and the distinction matters enough to state plainly:

- **Embedded is a technical tier, not a permission tier.** Anyone may write an embedded
  module. A community developer importing Sannad into their own binary and registering
  their own module is the framework's primary use case, not an exception to it. Sannad
  is a dependency in someone's `go.mod`, not a project they fork.

- **Two deployment contexts, two rules.** Self-hosted operators install any embedded
  module they choose, at their own risk. Sannad Cloud runs official modules plus
  approved third-party ones, and nothing else, in its own process.

- **Approval is a build-time gate, not a runtime one.** Go cannot dynamically load code,
  so an approved third-party embedded module is approved by being imported into the
  Cloud entrypoint and compiled into that binary. Two consequences follow and should not
  be discovered later: approved embedded modules ship on the Cloud's release cadence,
  and every approval is a deploy. A marketplace module that installs without a deploy is
  necessarily sandboxed or external.

This amendment does not change the decision. It states what the tiers were always
distinguishing, because the shorter phrasing invited the wrong reading.

Because verification is part of the plugin author's experience, it is treated as a
product surface with the same DX bar as the SDK itself: fast, automated, with
actionable failures rather than opaque rejection.

## Consequences

- Honest security posture. We do not claim isolation the architecture cannot deliver.
  Documentation must state plainly that embedded Go modules run with full process
  privileges.
- Self-hosted operators keep maximum power and maximum responsibility. This preserves
  the open-source promise and drives ecosystem growth.
- Sannad Cloud liability stays bounded, because the set of embedded code it executes
  is one we have verified.
- The same capability lives at any tier. A `parties.reader@v1` capability can be
  served by an embedded Go module, a WASM module, or a remote Salesforce-backed app,
  and callers cannot tell the difference. This is a property Odoo cannot offer.
- Cost: contracts must be serializable and tier-agnostic, which forbids `any`-typed
  handlers on cross-tier paths (see ADR 0003).
- Cost: three tiers mean three sets of documentation, tooling, and test harnesses.

## Alternatives considered

- **First-party and certified partners only for embedded Go.** Safest, and the
  honest match for a no-isolation tier. Rejected because it throttles the ecosystem
  growth that is the entire strategic goal, and because the verified-for-cloud gate
  achieves the same protection where it actually matters.
- **No in-process third-party code at all (pure Shopify model).** Strongest safety.
  Rejected because zero-latency embedded Go is a genuine differentiator against Odoo
  and a major DX draw for the Go community.
- **Single tier of external apps only.** Simplest to reason about. Rejected: it makes
  latency-critical extension impossible and abandons the performance advantage of a
  Go kernel.
