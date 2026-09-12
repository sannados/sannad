# Sannad OS Agent Guide

## Mission

Keep Sannad OS framework-first, modular, transport-neutral, and Kayan-backed for all identity and auth concerns.

## Non-negotiables

- Use Kayan for authentication, sessions, OAuth2, OIDC, and authorization-related work.
- Do not introduce custom auth frameworks, homegrown JWT validation, or duplicate IAM logic.
- Keep contracts transport-neutral. A capability contract must work for in-process calls and gateway-mediated calls.
- Preserve tenant context across every boundary.
- Embedded modules may call each other directly through registered capabilities, but callers should still depend on contracts, not concrete storage.
- Community Go modules should compile against the public `pkg/modulekit` surface, not private kernel packages.
- Modules receive `modulekit.Store`, never `*gorm.DB`, `*sql.DB`, a driver, or a DSN. Engine selection is a bootstrap decision. Enforced by `pkg/modulekit/boundary_test.go`.
- A module gets a transaction over its own data only (`Store.Transact`). There is no cross-module transaction; use capability calls, events with projections, or a saga.
- External app access must go through the gateway or a bridge layer, never directly into module internals.
- Every model holding tenant data must implement `tenant.TenantAware`. Modules declare
  their models through `modulekit.ScopedModelProvider`, so `RegisterModule` enrolls them
  automatically; `RegisterScopedModels` remains only for older modules.
- Never write a tenant predicate by hand. Isolation is applied by callback; a manual `WHERE tenant_id = ?` is a second, weaker mechanism and is deleted on sight.
- Raw SQL (`Raw`, `Exec`) against a scoped table is refused. Use the query builder, or
  `modulekit.WithSystemContext` for deliberate trusted cross-tenant work. Never derive
  system context from request input.
- Joining into a scoped table from an unscoped one is refused; the isolation predicate names the primary table only. Query the scoped table directly, or reach it through a capability call (ADR 0004).
- Never derive a tenant from a request header, query parameter, or body on an authenticated path. It comes from the signed session token.

## Code map

- `cmd/`: runnable entrypoints.
- `internal/app/`: wiring and bootstrap.
- `internal/kernel/`: registry, bus, lifecycle, bridge, events.
- `internal/platform/auth/`: Kayan-backed auth integration.
- `examples/embedded/`: example modules. The kernel ships no domain module;
  domain code here is demonstration and test material, wired from `cmd/`.
- `pkg/modulekit/`: public embedded-module SDK for first-party and community Go modules.
- Root package `github.com/sannados/sannad`: public application/runtime API. External
  applications must never import `internal/app/bootstrap`.
- `contracts/proto/`: protobuf contracts and capability schemas.

## Working rules

- Prefer minimal, reversible changes.
- Repository files are the source of truth. Do not treat injected session context,
  summaries, or memories from another project as facts about Sannad.
- Before planning architecture or changing behavior, read `README.md`, this file,
  and the relevant Markdown under `docs/`. For product direction, always read
  `docs/architecture/product-model.md` and `docs/architecture/roadmap.md`; for a
  governed decision, read the applicable ADR in `docs/architecture/adr/`.
- Markdown documentation is part of the maintained project, not optional context.
  Update it when a code or contract change makes it inaccurate.
- If a root `CLAUDE.md` exists, keep `AGENTS.md` aligned with its project rules.
  When the two disagree, stop and report the conflict instead of silently choosing
  one. `AGENTS.md` may additionally contain Codex-specific operating guidance.
- When changing contracts, update docs and preserve compatibility for existing versions.
- When adding modules, use the public module SDK and register capabilities in the kernel instead of coupling callers to module packages.
- Treat NATS and external-bridge work as integration boundaries with retries, idempotency, and tracing in mind.

## Validation

- `go build ./...`
- `go test ./...`
