# Module Model

Modules expose versioned capabilities, not direct database access.

## Embedded modules

- Written in Go.
- Registered in-process by the kernel.
- Can live in this repo or separate GitHub repos.
- Compile against the public `pkg/modulekit` package.
- Can call each other through capability handlers with transport-neutral contracts.
- Applications register them through the root `sannad.App`; module implementations
  depend only on `pkg/modulekit`.
- A module with tenant-owned models implements `modulekit.ScopedModelProvider`, so
  registration enrolls its models in isolation automatically.

## First-party embedded modules

- Maintained by the Sannad project.
- Live in their own repositories, compiled against `pkg/modulekit`. Example
  modules under `examples/embedded/` show the shape; the kernel ships no domain
  module of its own.

## Community embedded modules

- Maintained in separate repositories.
- Imported as normal Go dependencies.
- Installed into the kernel with zero network latency.
- Versioned against the public module SDK.

**Trust warning.** Embedded Go modules run in-process with full process privileges.
They can terminate the host process, read process memory including session secrets,
and bypass the capability registry by importing packages directly. No install-time
review changes this. Anyone may publish an embedded module and self-hosted operators
may install any of them, at their own risk, exactly as with any Go dependency.
Sannad Cloud executes only published and verified embedded modules. See
[ADR 0001](adr/0001-module-tiers-and-trust.md).

## Sandboxed WASM modules

- Compiled to WebAssembly, executed in-process under memory and time limits with no
  ambient filesystem or network access.
- Intended for latency-critical extension by untrusted authors.
- Provided capabilities are implemented today through operator-approved manifests and
  `sannad.App.RegisterWASM`. Outbound capability/event calls and hook subscriptions remain
  later ABI work. See
  [ADR 0001](adr/0001-module-tiers-and-trust.md) and
  [ADR 0003](adr/0003-extension-points-and-hooks.md).

## External apps

- Written in any language.
- Installed per tenant.
- Call into Sannad through the gateway using generated clients or REST.
- Receive async events through webhooks or future event subscriptions.

## Cross-tier communication

The three tiers speak the same capability and hook contracts, so a caller does not
know — and cannot tell — which tier serves a capability. See
[Plugin tiers and cross-tier communication](plugin-tiers.md) for worked samples of
each tier and every pairing between them.

## Reference example

`examples/embedded/crm` exposes `crm.contacts.reader@v1` and shows how an embedded
module installs a capability through the public module SDK. It is an example, not a
kernel deliverable: the kernel ships no domain module of its own.
