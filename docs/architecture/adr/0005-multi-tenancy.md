# ADR 0005: Multi-Tenancy Model

- Status: Accepted
- Date: 2026-08-17
- Supersedes: the "Proposed — decision required" draft of 2026-08-09

## Context

`AGENTS.md` states that tenant context must be preserved across every boundary, and
`modulekit.CallerInfo` already carries a `TenantID`. The isolation *strategy* was
never decided, and the gap is visible in the code as a live security defect
(recorded in full below).

The original draft of this ADR proposed building a tenancy layer: a tenant-scoped
repository, a tenant resolver, and cross-tenant escalation. Sannad already depends
on Kayan for identity, so the question became what Kayan already covers.

Implementation against `kayan/core@v0.1.0` established the actual split:

- **Kayan supplies resolution.** `core/tenant` provides context propagation
  (`WithTenantID`, `IDFromContext`), the `TenantAware` model interface, and a full
  set of request resolvers including `JWTClaimResolver`, `SubdomainResolver`, and
  `ChainResolver`.
- **Kayan does not supply storage enforcement.** There is no `kayan-gorm` module
  and no `gormstore` package; `kgorm@v0.1.0` contains no tenancy code at all. There
  is no `tenant.Scoper` and no `tenant.WithSystemContext`.

An earlier revision of this ADR named those three symbols as though they existed.
They do not, and the sections below are corrected.

Enforcement is therefore written in Sannad, in `internal/platform/tenancy`, against
Kayan's `TenantAware` interface. This is not a departure from the "adopt, do not
build" rule: that rule exists to keep Sannad from reimplementing *authentication*,
and this is storage plumbing bound to a Kayan interface, not a parallel identity
mechanism. Everything Kayan does provide is used as-is.

## The defect this ADR closes

Sannad currently derives the caller's tenant from client-controlled input.

`internal/app/bootstrap/app.go:466` — gRPC reads the tenant from client metadata:

```go
tenantValues := md.Get("x-tenant-id")
```

`internal/app/bootstrap/app.go:336` — HTTP lets a query parameter override it:

```go
tenantID := ctx.Query("tenant", caller.TenantID)
```

`examples/embedded/crm/module.go` then enforces:

```go
if hasCaller && caller.TenantID != "" && req.TenantID != caller.TenantID {
    return nil, fmt.Errorf("crm: tenant mismatch: %w", modulekit.ErrForbidden)
}
// ...
if req.TenantID != "" {
    q = q.Where("tenant_id = ?", req.TenantID)
}
```

Two distinct failures:

1. **The check compares two values from the same request.** An authenticated user
   sending `x-tenant-id: victim-tenant` satisfies the comparison, and the query runs
   `WHERE tenant_id = 'victim-tenant'`.
2. **An absent tenant disables enforcement entirely.** With no tenant supplied,
   `caller.TenantID` is `""`, the `caller.TenantID != ""` guard is false, the
   mismatch check is skipped, `req.TenantID` stays `""`, and the query runs with
   **no tenant predicate at all** — returning every tenant's contacts.

The root cause is upstream of both: `internal/platform/auth/identity.go` defines
`Account{ID, Email, PasswordHash, Traits}` with **no tenant membership field**. Even
if the gateway wanted to validate a client-supplied tenant, there is nothing to
validate it against.

Kayan's own documentation names this exact pattern as unsafe. On `HeaderResolver`:

> A header is client-supplied. Anyone who can reach your service can set
> `X-Tenant-ID` to any value. This resolver is safe behind a gateway that overwrites
> the header from an authenticated source, and unsafe on an endpoint a browser can
> reach directly.

Sannad uses the header and query forms with no such gateway.

## Decision

### 1. Adopt Kayan tenancy; do not build a parallel mechanism

Sannad uses `kayan/core/tenant` for resolution, context propagation, and the
`TenantAware` model interface. Sannad writes no resolver, no tenant-scoped
repository, and no identity mechanism of its own.

Storage enforcement lives in `internal/platform/tenancy` because Kayan does not
provide it. It is deliberately thin: GORM callbacks keyed on Kayan's `TenantAware`
interface, with no state and no schema of its own.

The split this ADR needs still holds: **resolution** (whose request is this) is
request-scoped and comes from `core/tenant`; **isolation** (this query cannot
return anyone else's rows) is a storage concern and lives in the bootstrap layer.
The split matters because the two fail differently — a resolution bug is loud and
an isolation bug is silent.

### 2. Isolation is enforced by callback, never per query

`tenancy.RegisterIsolation(db)` is called once at bootstrap. It registers GORM
callbacks on Query, Row, Update, Delete, and Create. After it:

- reads, updates, and deletes on models implementing `tenant.TenantAware` gain a
  `tenant_id` predicate automatically;
- inserts are stamped with the ambient tenant, and an insert carrying a *different*
  tenant is rejected with `ErrTenantMismatch`;
- **a scoped query with no tenant in the context returns `ErrNoTenantContext`** — it
  does not run unscoped and does not return empty.

The predicate is table-qualified, because an unqualified `tenant_id` becomes
ambiguous the moment a query joins two scoped tables. It is added as a
`clause.Where`, which GORM ANDs onto any predicate the caller wrote, so a caller
naming another tenant gets the empty set rather than an override — and a caller's
`OR` cannot widen the scope, because their whole expression is one ANDed operand.

Two paths do not fit that model, and adversarial testing found both leaking every
tenant's rows before they were closed:

- **Queries that name a table as a string** — `Table("contacts")` — carry no model
  type, so the `TenantAware` check has nothing to inspect. `RegisterScopedModels`
  records the table name of every scoped model at bootstrap, and the table name is
  used as a fallback. A scoped model missing from that list is still protected on
  every typed path; only the string-addressed path degrades.
- **Raw SQL** — `Raw`, `Exec`, and `Raw().Scan()`/`.Row()`, which reach the query
  callback with pre-built SQL — cannot have a predicate injected at all. These are
  **refused**, not scoped, whenever the statement mentions a scoped table.
- **Joins into a scoped table from an unscoped one** — the predicate names the
  *primary* table, so `Table("global_records").Joins("CROSS JOIN contacts")` read
  every tenant's contacts. Also **refused**: deriving the correct predicate from an
  arbitrary join expression is not something to get subtly wrong. A scoped table
  joining *outward* to an unscoped one is unaffected and stays scoped.

Raw SQL is refused *even when a tenant is in context*. Treating "a tenant exists" as
sufficient would accept precisely the unscoped hand-written query the guard exists to
catch, since the statement is opaque either way. Scoped work belongs on the query
builder; deliberate cross-tenant raw SQL goes under `WithSystemContext`.

These are the places the design trades ergonomics for safety: a legitimately scoped
raw query must be rewritten on the builder or declared as a system operation, and a
cross-module join must go through a capability call or a projection instead. Both
costs are accepted — raw SQL and hand-written joins are exactly where cross-tenant
reporting queries get written — and the second is already required by ADR 0004,
which forbids cross-module joins for unrelated reasons.

Per-method predicates are prohibited. The reasoning is Kayan's and it is correct:
the one query somebody forgets is the one that leaks, and a forgotten predicate
produces no error, no failing test, and no log entry. A callback inverts which
mistake is possible — forgetting produces an error, and crossing a boundary
requires explicitly typing `tenancy.WithSystemContext`.

Concretely, `q.Where("tenant_id = ?", req.TenantID)` in `crm/module.go` is deleted
rather than corrected. Leaving a manual check beside callback enforcement means two
mechanisms, and the weaker one is the one future module authors copy.

### 3. Tenant is bound to the authenticated session

The resolver chain is ordered by trust, and order is a security decision:

```go
tenant.NewChainResolver(
    tenant.NewJWTClaimResolver("tenant_id", claimsContextKey),
    tenant.NewSubdomainResolver(baseDomain),
)
```

`JWTClaimResolver` is first because the tenant is then bound to the same signature
that authenticated the user — a client cannot change it without invalidating their
session.

**`HeaderResolver` and `QueryResolver` are not in the chain.** The `x-tenant-id`
metadata read and the `?tenant=` query parameter are removed from Sannad entirely.
They may be reintroduced only behind a gateway that overwrites the value from an
authenticated source, and that would be a change to this ADR.

This requires two upstream changes:

- `Account` gains tenant membership, so there is something to authorize against.
- The session token carries a `tenant_id` claim, signed by the HS256 session
  strategy already configured in `internal/platform/auth/service.go`.

Resolution decides which tenant a request is about. It does not authenticate that
the caller may act for it — membership does. Both are required.

### 4. Resolve once, at the edge

One middleware, before anything touches storage, and it must propagate the returned
context. `Manager.Resolve` returns a new context and discarding it compiles fine
while leaving the tenant unset — every scoped query beneath then fails closed, which
is the design working but with a misleading cause.

Background work is either a per-tenant loop with `WithTenantID` (preferred, because
a bug then affects one tenant) or an explicit `tenancy.WithSystemContext`.

### 5. Isolation model: row-level now, stronger models later without module changes

Row-level isolation — `tenant_id` on shared tables — is what
`tenancy.RegisterIsolation` implements and what Sannad ships.

The original draft's options B (schema-per-tenant) and C (database-per-tenant)
remain reachable, but they are **not a configuration flag in this design**. The seam
is `internal/platform/tenancy` itself: modules never name a tenant, so how isolation
is achieved is invisible to them. A schema- or database-per-tenant model changes how
`RegisterIsolation` resolves a connection and changes nothing above it.

Deferring stays cheap, but the seam is now Sannad's to maintain rather than a
dependency's — the cost of enforcement not existing upstream.

### 6. The GORM adapter is bootstrap-layer, not kernel-layer

`internal/platform/tenancy` depends on GORM, and that dependency must not contradict
the database-agnostic goal.

The boundary: **bootstrap selects the adapter; modules receive a scoped handle and
never see GORM.** This mirrors Kayan's own structure — `core/` is storage-neutral and
each binding is separate. A deployment on another engine replaces the enforcement
package with an equivalent for that engine; module code is unchanged, because
modules never express a tenant predicate in the first place.

The mechanics of that handle are ADR 0006's subject. This ADR only fixes the layer
the dependency is allowed to live in.

## Consequences

- The cross-tenant defect is closed at the mechanism level, not patched at the call
  site. A future module that forgets to filter gets an error, not a leak.
- Modules never see connection strings or tenant predicates. Every model that belongs
  to a tenant implements `tenant.TenantAware`, and that is the module author's whole
  obligation.
- **A model that is not marked `TenantAware` has no isolation and nothing reports it.**
  This is the one remaining silent failure and it is a per-model decision, so it
  belongs in the module review checklist and in the SDK documentation.
- Cross-tenant operations require `tenancy.WithSystemContext`, which is greppable.
  Every call site is security-relevant and reviewable, and a system context on a
  request-handling path removes isolation from everything downstream of it.
- `Account` gaining tenant membership is a change to the identity model and touches
  registration, login, and session issuance.
- The tenant column must be indexed on every scoped table, since every query in the
  system now carries a predicate on it.
- Cost: Sannad owns and maintains the enforcement callbacks, because Kayan does not
  provide them. Bounded by the layering rule in decision 6, and by the negative
  control in Verification, which fails if the callbacks stop being what enforces
  scoping.

## Verification

The adversarial test is a requirement, not a suggestion: create two tenants, insert
under one, read under the other, and assert **both** that the read is refused and
that zero rows come back. Asserting only `err != nil` can pass for an unrelated
reason.

Implemented in `internal/platform/tenancy/isolation_test.go`, covering reads,
cross-tenant predicates, primary-key fetches, updates, deletes, insert stamping, and
insert smuggling; and end to end in
`TestGateway_CRMContacts_CannotReachAnotherTenant`, which replays the original
attack — `X-Tenant-ID` and `?tenant=` both naming a foreign tenant — and asserts on
the returned rows.

`TestIsolationIsWhatEnforcesScoping` is the negative control the "remove it and see
it fail" instruction asks for, kept as a permanent test rather than a manual step:
it runs the same cross-tenant read against a database with no isolation registered
and asserts the leak occurs. If it ever passes, the other tests are green for some
reason other than the callbacks and their guarantee is not real.

The no-tenant case is tested separately, because it is the more dangerous of the
two failure modes described above:

```go
func TestUnscopedContextFails(t *testing.T) {
    ctx := context.Background() // deliberately no tenant

    var got []scopedRecord
    err := db.WithContext(ctx).Find(&got).Error

    if !errors.Is(err, tenancy.ErrNoTenantContext) {
        t.Fatalf("expected ErrNoTenantContext, got %v", err)
    }
    if len(got) != 0 {
        t.Fatalf("unscoped query returned %d rows: %+v", len(got), got)
    }
}
```

## Alternatives considered

- **Build Sannad's own tenant-scoped repository (the original draft).** Rejected:
  the repository pattern puts isolation on a path module authors can step around,
  whereas a callback on the shared connection has no such path. Sannad writes the
  callbacks because Kayan has none, but keeps Kayan's resolution, context, and
  `TenantAware` interface rather than duplicating them.
- **Explicit tenant scoping in each module.** Rejected: it restores exactly the
  failure mode this ADR exists to close — a forgotten predicate produces no error,
  no failing test, and no log entry.
- **Fix the comparison in `crm/module.go` and keep the current shape.** Rejected: it
  addresses one call site in one module while leaving the mechanism — per-method
  predicates and a client-supplied tenant — intact for every module written after it.
- **Keep `HeaderResolver` for service-to-service calls.** Rejected for now: it is safe
  only behind a gateway that overwrites the header from an authenticated source, and
  Sannad has no such gateway today. Service-to-service tenancy is deferred to whenever
  service identity is designed, rather than being approximated with a trusted header.
- **Schema-per-tenant or database-per-tenant from the start.** Rejected as premature:
  both are reachable later through `Scoper` without touching module code, so the
  usual "expensive to retrofit" argument does not apply here.
