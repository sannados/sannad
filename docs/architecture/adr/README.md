# Architecture Decision Records

Records of significant architectural decisions for Sannad OS: the context, the
decision, its consequences, and the alternatives that were rejected.

| ADR | Title | Status |
|---|---|---|
| [0001](0001-module-tiers-and-trust.md) | Module Tiers and Trust Model | Accepted |
| [0002](0002-external-api-surface.md) | External API Surface | Accepted |
| [0003](0003-extension-points-and-hooks.md) | Extension Points and Hooks | Accepted |
| [0004](0004-cross-module-data-access.md) | Cross-Module Data Access and Shared References | Accepted |
| [0005](0005-multi-tenancy.md) | Multi-Tenancy Model | Accepted |
| [0006](0006-storage-boundary-and-transactions.md) | Storage Boundary and Transaction Semantics | Accepted |
| [0007](0007-sandbox-permissions-and-host-calls.md) | Sandbox Permissions and Host-Call ABI | Accepted |

## The through-line

One idea connects these records: **a contract is declared once and may be served at
any tier.**

A capability or hook point does not know whether it is answered by a trusted
in-process Go module, a sandboxed WASM module, or a remote third-party app. That
symmetry is what allows the ecosystem to be simultaneously open (anyone may publish),
safe (Sannad Cloud runs only verified or sandboxed code), and fast (first-party and
trusted code pays no serialization cost).

Everything else follows from it:

- ADR 0001 defines the tiers.
- ADR 0002 defines how remote tiers reach the kernel.
- ADR 0003 defines how any tier participates in flows it does not own, and why
  cross-tier contracts must be typed rather than `any`.
- ADR 0004 defines who owns data and how other modules reach it without coupling to
  storage.
- ADR 0005 defines the isolation boundary all of the above operate within.
- ADR 0006 defines where engine types may appear and what consistency a module gets,
  which is what keeps that boundary implementable on more than one engine.
- ADR 0007 defines how sandboxed modules consume those contracts without gaining
  ambient access, while preserving ABI v1.

Two of these constrain when work may happen rather than only how. ADR 0005 must land
before modules are written, because storage has to be safe before it is abstracted.
ADR 0006 must land before modules are *generated*, because the transaction contract
is what every generated module is written against.

The build order those constraints imply is recorded in
[the implementation roadmap](../roadmap.md).
